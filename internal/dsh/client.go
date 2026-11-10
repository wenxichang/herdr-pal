package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"unicode"
)

const (
	// sessionListRawLimit 限制一次 session/list 返回的原始行数，防止异常响应耗尽内存。
	sessionListRawLimit = 8192
	// sessionListLimit 限制过滤后每个宿主可控制的会话数，超出即失败而不是截断。
	sessionListLimit = 256
	// sessionIDMaxBytes 限制会话 ID 长度；ID 会进入目标映射，必须有限。
	sessionIDMaxBytes = 512
)

var (
	errSessionListMalformed = errors.New("dsh 会话目录格式无效")
	errSessionListTooMany   = errors.New("dsh 会话目录超过 8192 条限制")
	errSessionCapacity      = errors.New("dsh 可控制会话超过 256 条限制")
	errSessionIDInvalid     = errors.New("dsh 会话 ID 无效")
	errSessionIDDuplicate   = errors.New("dsh 会话 ID 重复")
)

// Client 是单个 dsh 宿主的高层客户端，负责目录、正文和交互操作。
//
// 每个 Client 只绑定一个已认证的本机宿主；宿主重启后应丢弃并重新发现。
type Client struct {
	host   Host
	rpc    *rpcClient
	logger *slog.Logger
}

// SessionSummary 是会话目录中的一个可控制会话。
//
// Available 固定为 true：不可控制的冷历史和子代理会话不会出现在目录中。
type SessionSummary struct {
	// ID 是 dsh 原生会话 ID，非空且不含控制字符。
	ID string
	// Title 取投影标题；标题缺失时回退为 ID。
	Title string
	// Cwd 保留 dsh 返回的原始工作目录，可能为空。
	Cwd string
	// Running 表示 Agent 当前是否正在执行任务。
	Running bool
	// Available 表示当前宿主持有该会话的 Agent 控制权。
	Available bool
	// UpdatedAt 是持久化活动时间戳，用于目录排序。
	UpdatedAt int64
}

// NewClient 为已发现的宿主创建高层客户端。
//
// logger 可为 nil；客户端只在需要诊断时记录不含认证材料与会话正文的信息。
func NewClient(host Host, logger *slog.Logger) *Client {
	return &Client{
		host:   host,
		rpc:    newRPCClient(host),
		logger: logger,
	}
}

// sessionListValue 是 session/list 返回值的外层结构。
type sessionListValue struct {
	Items []json.RawMessage `json:"items"`
}

// sessionListRow 是 session/list 的单行原始数据；只解析本模块使用的字段。
type sessionListRow struct {
	SessionID      string `json:"sessionId"`
	AgentAvailable *bool  `json:"agentAvailable"`
	FormatStatus   string `json:"formatStatus"`
	Origin         string `json:"origin"`
	Cwd            string `json:"cwd"`
	UpdatedAt      int64  `json:"updatedAt"`
	Running        bool   `json:"running"`
	Projections    struct {
		Values struct {
			Title string `json:"title"`
		} `json:"values"`
	} `json:"projections"`
}

// Sessions 返回当前宿主真正持有 Agent 控制权的会话。
//
// 冷历史、子代理会话和迁移中的会话会被过滤；结果按 UpdatedAt 降序、
// ID 升序稳定排序。ID 无效、重复或数量超限时整体失败，不返回部分目录。
func (c *Client) Sessions(ctx context.Context) ([]SessionSummary, error) {
	raw, err := c.rpc.call(ctx, "session/list", map[string]any{"_request": map[string]any{}})
	if err != nil {
		return nil, fmt.Errorf("读取 dsh 会话目录失败: %w", err)
	}

	var value sessionListValue
	if err := json.Unmarshal(raw, &value); err != nil || value.Items == nil {
		return nil, errSessionListMalformed
	}
	if len(value.Items) > sessionListRawLimit {
		return nil, errSessionListTooMany
	}

	sessions := make([]SessionSummary, 0, min(len(value.Items), sessionListLimit))
	seen := make(map[string]struct{}, len(value.Items))
	for _, item := range value.Items {
		var row sessionListRow
		if err := json.Unmarshal(item, &row); err != nil {
			return nil, fmt.Errorf("%w: %v", errSessionListMalformed, err)
		}
		if err := validateSessionID(row.SessionID); err != nil {
			return nil, err
		}
		if _, duplicated := seen[row.SessionID]; duplicated {
			return nil, fmt.Errorf("%w: %q", errSessionIDDuplicate, row.SessionID)
		}
		seen[row.SessionID] = struct{}{}

		// 只有宿主确认持有 Agent 的会话才可控制；缺失该字段时按不可控制处理。
		if row.AgentAvailable == nil || !*row.AgentAvailable {
			continue
		}
		if row.Origin == "subagent" || row.FormatStatus == "migration-required" {
			continue
		}
		if len(sessions) >= sessionListLimit {
			return nil, errSessionCapacity
		}

		title := row.Projections.Values.Title
		if title == "" {
			title = row.SessionID
		}
		sessions = append(sessions, SessionSummary{
			ID:        row.SessionID,
			Title:     title,
			Cwd:       row.Cwd,
			Running:   row.Running,
			Available: true,
			UpdatedAt: row.UpdatedAt,
		})
	}

	// ID 已保证唯一，因此 UpdatedAt 降序、ID 升序的比较器构成全序，排序结果完全确定。
	sort.SliceStable(sessions, func(i, j int) bool {
		if sessions[i].UpdatedAt != sessions[j].UpdatedAt {
			return sessions[i].UpdatedAt > sessions[j].UpdatedAt
		}
		return sessions[i].ID < sessions[j].ID
	})
	return sessions, nil
}

const (
	// readFastMaxMessages 是尾部快路径一次请求的消息上限。
	readFastMaxMessages = 100
	// readFallbackMaxMessages 是降级分页时单页的消息上限。
	readFallbackMaxMessages = 32
	// readFallbackMaxPages 限制降级分页最多向更早历史追读的页数。
	readFallbackMaxPages = 4
	// readTargetMessages 是最终展示窗口的消息条数，追读满该数量即可停止。
	readTargetMessages = 100
)

var (
	// errHistoryUnavailable 表示会话尚未完成顺序投影，无法只读访问历史。
	errHistoryUnavailable = errors.New("dsh 历史暂不可读，请在原生界面完成初始化或迁移")
	// errHistoryTooLarge 表示历史超过读取预算；调用方不得展示半截正文。
	errHistoryTooLarge = errors.New("dsh 历史过大，无法读取")
	// errPageMalformed 表示 session/page 返回值缺少分页字段。
	errPageMalformed = errors.New("dsh 历史分页格式无效")
)

// projectionsRequest 是 session/projections 的请求参数。
type projectionsRequest struct {
	SessionID string `json:"sessionId"`
}

// projectionsArgs 对应远程 API 的 {"request": ...} 外层。
type projectionsArgs struct {
	Request projectionsRequest `json:"request"`
}

// sessionAddress 标识 session/page 的目标会话。
type sessionAddress struct {
	Kind      string `json:"kind"`
	SessionID string `json:"sessionId"`
}

// pageRequest 是 session/page 的请求参数。
type pageRequest struct {
	Address     sessionAddress `json:"address"`
	ThroughSeq  int64          `json:"throughSeq"`
	BeforeSeq   *uint64        `json:"beforeSeq,omitempty"`
	MaxMessages int            `json:"maxMessages"`
}

// pageArgs 对应远程 API 的 {"request": ...} 外层。
type pageArgs struct {
	Request pageRequest `json:"request"`
}

// sessionPage 是 session/page 返回值中本模块使用的字段。
type sessionPage struct {
	records []json.RawMessage
	hasMore bool
}

// Read 读取会话已持久化的聊天正文。
//
// 先以 session/projections 取得顺序投影切点，再用 session/page 读取尾页；
// 尾页响应超过 RPC 上限或分页不可信时改为 32 条一页向上追读，最多 4 页或
// 100 条消息。仍无法完整读取时返回 errHistoryTooLarge，不返回部分正文。
func (c *Client) Read(ctx context.Context, sessionID string) (string, error) {
	if err := validateSessionID(sessionID); err != nil {
		return "", err
	}
	asOfSeq, err := c.sequencedCut(ctx, sessionID)
	if err != nil {
		return "", err
	}

	raw, err := c.rpc.call(ctx, "session/page", pageArgs{Request: pageRequest{
		Address:     sessionAddress{Kind: "session", SessionID: sessionID},
		ThroughSeq:  asOfSeq,
		MaxMessages: readFastMaxMessages,
	}})
	if err == nil {
		page, parseErr := parseSessionPage(raw)
		if parseErr == nil {
			text, transcriptErr := conversationText(page.records)
			if transcriptErr != nil {
				return "", fmt.Errorf("读取 dsh 会话正文失败: %w", transcriptErr)
			}
			return text, nil
		}
		err = parseErr
	}
	// 只有分页层面的失败才值得降级；认证、网络或业务错误直接失败，不重复请求。
	if !isPageFallbackError(err) {
		return "", fmt.Errorf("读取 dsh 会话正文失败: %w", err)
	}
	return c.readFallback(ctx, sessionID, asOfSeq)
}

// sequencedCut 返回可比较的顺序投影切点；非 sequenced 或切点无效时失败关闭。
func (c *Client) sequencedCut(ctx context.Context, sessionID string) (int64, error) {
	raw, err := c.rpc.call(ctx, "session/projections", projectionsArgs{
		Request: projectionsRequest{SessionID: sessionID},
	})
	if err != nil {
		return 0, fmt.Errorf("读取 dsh 会话投影失败: %w", err)
	}

	var value struct {
		Kind    string `json:"kind"`
		AsOfSeq *int64 `json:"asOfSeq"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, fmt.Errorf("%w: 投影格式无效", errHistoryUnavailable)
	}
	if value.Kind != "sequenced" || value.AsOfSeq == nil || *value.AsOfSeq < -1 {
		return 0, errHistoryUnavailable
	}
	return *value.AsOfSeq, nil
}

// parseSessionPage 解析分页返回值；records 与 hasMore 缺失都视为不可信分页。
func parseSessionPage(raw json.RawMessage) (sessionPage, error) {
	var value struct {
		Records []json.RawMessage `json:"records"`
		HasMore *bool             `json:"hasMore"`
	}
	if err := json.Unmarshal(raw, &value); err != nil || value.Records == nil || value.HasMore == nil {
		return sessionPage{}, errPageMalformed
	}
	return sessionPage{records: value.Records, hasMore: *value.HasMore}, nil
}

// isPageFallbackError 判断快路径失败是否可以由小页分页重试恢复。
func isPageFallbackError(err error) bool {
	return errors.Is(err, errRPCResponseTooLarge) || errors.Is(err, errPageMalformed)
}

// readFallback 从切点向上按 32 条一页追读，直到历史读完或已覆盖最近 100 条消息。
//
// 任何无法确认完整性的分页结果都返回 errHistoryTooLarge，避免把半截历史
// 当成完整正文展示。页面按从新到旧收集，投影前再恢复为时间顺序。
func (c *Client) readFallback(ctx context.Context, sessionID string, throughSeq int64) (string, error) {
	var newestFirst [][]json.RawMessage
	var beforeSeq *uint64
	for pageIndex := 0; pageIndex < readFallbackMaxPages; pageIndex++ {
		raw, err := c.rpc.call(ctx, "session/page", pageArgs{Request: pageRequest{
			Address:     sessionAddress{Kind: "session", SessionID: sessionID},
			ThroughSeq:  throughSeq,
			BeforeSeq:   beforeSeq,
			MaxMessages: readFallbackMaxMessages,
		}})
		if err != nil {
			if errors.Is(err, errRPCResponseTooLarge) {
				return "", fmt.Errorf("%w: 单页响应超过 RPC 上限", errHistoryTooLarge)
			}
			return "", fmt.Errorf("读取 dsh 会话正文失败: %w", err)
		}

		page, parseErr := parseSessionPage(raw)
		if parseErr != nil {
			return "", fmt.Errorf("%w: %v", errHistoryTooLarge, parseErr)
		}
		newestFirst = append(newestFirst, page.records)
		if !page.hasMore {
			return projectPages(newestFirst)
		}

		first, seqErr := firstRecordSeq(page.records)
		if seqErr != nil {
			return "", fmt.Errorf("%w: %v", errHistoryTooLarge, seqErr)
		}
		if beforeSeq != nil && first >= *beforeSeq {
			return "", fmt.Errorf("%w: 分页游标未推进", errHistoryTooLarge)
		}
		count, countErr := projectedMessageCount(newestFirst)
		if countErr != nil {
			return "", fmt.Errorf("读取 dsh 会话正文失败: %w", countErr)
		}
		// 更早的事件不可能进入最近 100 条窗口，已覆盖时无需继续追读。
		if count >= readTargetMessages {
			return projectPages(newestFirst)
		}
		if pageIndex == readFallbackMaxPages-1 {
			return "", fmt.Errorf("%w: 超过 %d 页仍未读完", errHistoryTooLarge, readFallbackMaxPages)
		}
		beforeSeq = &first
	}
	return "", errHistoryTooLarge
}

// firstRecordSeq 返回一页记录中的最小事件序号，用于计算下一页的 beforeSeq。
func firstRecordSeq(records []json.RawMessage) (uint64, error) {
	var first uint64
	found := false
	for _, raw := range records {
		var record transcriptRecord
		if err := json.Unmarshal(raw, &record); err != nil || record.Event.Seq == nil {
			return 0, errPageMalformed
		}
		if seq := *record.Event.Seq; !found || seq < first {
			first = seq
			found = true
		}
	}
	if !found {
		return 0, errPageMalformed
	}
	return first, nil
}

// flattenPages 把从新到旧收集的页面恢复为按事件时间顺序排列的记录。
func flattenPages(newestFirst [][]json.RawMessage) []json.RawMessage {
	records := make([]json.RawMessage, 0)
	for index := len(newestFirst) - 1; index >= 0; index-- {
		records = append(records, newestFirst[index]...)
	}
	return records
}

// projectedMessageCount 统计当前已收集页面投影后的可见消息数。
func projectedMessageCount(newestFirst [][]json.RawMessage) (int, error) {
	messages, err := projectMessages(flattenPages(newestFirst))
	if err != nil {
		return 0, err
	}
	return len(messages), nil
}

// projectPages 按时间顺序投影已收集页面。
func projectPages(newestFirst [][]json.RawMessage) (string, error) {
	text, err := conversationText(flattenPages(newestFirst))
	if err != nil {
		return "", fmt.Errorf("读取 dsh 会话正文失败: %w", err)
	}
	return text, nil
}

// validateSessionID 要求会话 ID 非空、不超过 512 字节且不含控制字符。
// 会话 ID 会进入日志与目标映射，因此不接受任何需要转义的字符。
func validateSessionID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: ID 为空", errSessionIDInvalid)
	}
	if len(id) > sessionIDMaxBytes {
		return fmt.Errorf("%w: 超过 %d 字节", errSessionIDInvalid, sessionIDMaxBytes)
	}
	if strings.IndexFunc(id, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: 含控制字符", errSessionIDInvalid)
	}
	return nil
}
