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
