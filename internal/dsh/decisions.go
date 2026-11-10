package dsh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// decisionMaxQuestions 限制单个决策的问题数，避免异常原生表单耗尽展示预算。
	decisionMaxQuestions = 32
	// decisionMaxOptions 限制单题选项数。
	decisionMaxOptions = 32
	// decisionTextMaxBytes 限制问题标题、描述与选项展示文本。
	decisionTextMaxBytes = 16 * 1024
	// decisionIDMaxBytes 限制问题 ID、callId 等会进入回答载荷的标识。
	decisionIDMaxBytes = 512
	// decisionStateReasonMaxBytes 限制状态说明中回显的原生状态文本。
	decisionStateReasonMaxBytes = 64
)

const (
	// decisionKindApproval 表示工具调用审批。
	decisionKindApproval = "approval"
	// decisionKindForm 表示结构化问题表单。
	decisionKindForm = "form"
	// decisionKindUnsupported 表示保留事实但无法远程回答的交互。
	decisionKindUnsupported = "unsupported"
)

const (
	// questionKindApproval 是工具审批的固定问题类型。
	questionKindApproval = "approval"
	// questionKindSingle 是单选问题。
	questionKindSingle = "single"
	// questionKindMultiple 是多选问题。
	questionKindMultiple = "multiple"
	// questionKindText 是纯文本问题。
	questionKindText = "text"
)

var (
	// errDecisionMalformed 表示决策来源结构不符合 dsh 协议；调用方应失败关闭。
	errDecisionMalformed = errors.New("dsh 决策格式无效")
)

// Decision 表示一条可通过远程回答的 dsh 原生交互，或一条仅保留事实的未知交互。
//
// NativeID 使用 event:<eventId>（Mux waterfall）或 call:<callId>（持续问题）标识；
// Revision 是原始帧内容的 sha256，用于提交前复核决策未被替换。
type Decision struct {
	// NativeID 是 dsh 原生交互标识。
	NativeID string
	// SessionID 是产生该交互的 dsh 会话。
	SessionID string
	// Kind 取 approval、form 或 unsupported。
	Kind string
	// Title 是面向用户的决策标题。
	Title string
	// Questions 是按原生顺序排列的问题；unsupported 决策通常为空。
	Questions []Question
	// Revision 是原始帧/投影条目的 sha256 十六进制摘要。
	Revision string
	// Blocking 表示该决策是否应进入 blocked 状态并提示用户回答。
	Blocking bool
	// Supported 表示是否支持通过 IM 远程回答。
	Supported bool
	// Reason 解释不支持远程回答的原因。
	Reason string
}

// Question 是决策中的一个问题。
//
// Kind 取 approval、single、multiple 或 text；选项的 Value 是提交给 dsh 的原生值，
// Label 是可以安全展示给 IM 用户的文本。
type Question struct {
	// ID 是原生问题 ID，回答时必须原样带回。
	ID string
	// Title 是问题正文。
	Title string
	// Description 是问题补充说明。
	Description string
	// Kind 是问题类型。
	Kind string
	// Options 是可选答案；文本问题为空。
	Options []Option
	// Required 表示该问题必须回答；当前 dsh 问题均为必答。
	Required bool
}

// Option 是一个可选项。
type Option struct {
	// Value 是提交给 dsh 的原生选项值。
	Value string
	// Label 是展示文本，已去除控制字符与 ANSI 转义。
	Label string
	// Description 是选项说明。
	Description string
}

// approvalRequestPayload 是 approval/request 瀑布请求中本模块使用的字段。
type approvalRequestPayload struct {
	ToolName      string            `json:"toolName"`
	Reason        string            `json:"reason"`
	DisplayReason map[string]string `json:"displayReason"`
}

// askUserQuestionItem 是 dsh 原生问题条目的公开字段。
type askUserQuestionItem struct {
	ID          string                  `json:"id"`
	Question    string                  `json:"question"`
	Detail      string                  `json:"detail"`
	Header      string                  `json:"header"`
	Options     []askUserQuestionOption `json:"options"`
	MultiSelect bool                    `json:"multiSelect"`
}

// askUserQuestionOption 是 dsh 原生选项；原生只记录 label，没有独立 value。
type askUserQuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// userQuestionsRequestPayload 是 user-questions/request 瀑布请求中本模块使用的字段。
type userQuestionsRequestPayload struct {
	Questions []askUserQuestionItem `json:"questions"`
	Wait      *struct {
		CallID string `json:"callId"`
		Timed  bool   `json:"timed"`
	} `json:"wait"`
}

// continuedQuestionEntry 是 projections.values.userQuestions.active 的单条记录。
type continuedQuestionEntry struct {
	CallID    string                `json:"callId"`
	State     string                `json:"state"`
	Questions []askUserQuestionItem `json:"questions"`
}

// decisionsFrom 合并 Mux 待答 waterfall 与会话投影中的持续问题。
//
// 同一会话的审批与问题按 waterfall 到达顺序在前，投影中的持续问题在后；
// 已在 waterfall（timed 问题）出现的 callId 不会重复生成投影决策。
// 任一来源结构非法时整体返回错误，不返回部分决策。
func decisionsFrom(sessionID string, pending PendingSnapshot, projections json.RawMessage) ([]Decision, error) {
	pendingDecisions, callIDs, err := decisionsFromPending(sessionID, pending)
	if err != nil {
		return nil, err
	}
	projectionDecisions, err := decisionsFromProjections(sessionID, projections, callIDs)
	if err != nil {
		return nil, err
	}
	decisions := make([]Decision, 0, len(pendingDecisions)+len(projectionDecisions))
	decisions = append(decisions, pendingDecisions...)
	decisions = append(decisions, projectionDecisions...)
	return decisions, nil
}

// decisionsFromPending 把属于 sessionID 的 Mux 待答项转换为决策。
//
// 返回的 callId 集合用于跳过投影中重复的 timed 问题；其他会话的待答项被忽略。
func decisionsFromPending(sessionID string, pending PendingSnapshot) ([]Decision, map[string]struct{}, error) {
	if err := validateDecisionIdentifier("sessionId", sessionID); err != nil {
		return nil, nil, err
	}
	decisions := make([]Decision, 0, len(pending.Items))
	callIDs := make(map[string]struct{})
	for _, item := range pending.Items {
		if item.AgentID != sessionID {
			continue
		}
		if err := validateDecisionIdentifier("eventId", item.EventID); err != nil {
			return nil, nil, err
		}

		switch item.Event {
		case "approval/request":
			decision, err := approvalDecision(item, sessionID)
			if err != nil {
				return nil, nil, err
			}
			decisions = append(decisions, decision)
		case "user-questions/request":
			decision, callID, err := formDecision(item, sessionID)
			if err != nil {
				return nil, nil, err
			}
			if callID != "" {
				callIDs[callID] = struct{}{}
			}
			decisions = append(decisions, decision)
		default:
			decision, err := unsupportedWaterfallDecision(item, sessionID)
			if err != nil {
				return nil, nil, err
			}
			decisions = append(decisions, decision)
		}
	}
	return decisions, callIDs, nil
}

// approvalDecision 把 approval/request 转换为固定两选项的 blocking 审批。
func approvalDecision(item PendingItem, sessionID string) (Decision, error) {
	var payload approvalRequestPayload
	request := bytes.TrimSpace(item.Request)
	if len(request) > 0 {
		if err := json.Unmarshal(request, &payload); err != nil {
			return Decision{}, fmt.Errorf("%w: 审批请求格式无效: %v", errDecisionMalformed, err)
		}
	}
	revision, err := decisionRevision(item.Raw)
	if err != nil {
		return Decision{}, err
	}

	toolName := sanitizeDecisionText(payload.ToolName, decisionTextMaxBytes)
	title := "工具调用审批"
	if toolName != "" {
		title = "审批：" + toolName
	}
	reason := sanitizeDecisionText(payload.Reason, decisionTextMaxBytes)
	if reason == "" {
		reason = sanitizeDecisionText(preferredDisplayReason(payload.DisplayReason), decisionTextMaxBytes)
	}
	return Decision{
		NativeID:  "event:" + item.EventID,
		SessionID: sessionID,
		Kind:      decisionKindApproval,
		Title:     title,
		Questions: []Question{{
			ID:          "approval",
			Title:       "是否允许本次工具调用？",
			Description: reason,
			Kind:        questionKindApproval,
			Options:     approvalOptions(),
			Required:    true,
		}},
		Revision:  revision,
		Blocking:  true,
		Supported: true,
	}, nil
}

// formDecision 把 user-questions/request 转换为表单决策。
//
// request.wait.timed=true 的问题允许原生界面先行超时，因此不进入 blocking；
// wait.callId 用于与投影中的同一持续问题去重。
func formDecision(item PendingItem, sessionID string) (Decision, string, error) {
	var payload userQuestionsRequestPayload
	request := bytes.TrimSpace(item.Request)
	if len(request) > 0 {
		if err := json.Unmarshal(request, &payload); err != nil {
			return Decision{}, "", fmt.Errorf("%w: 问题请求格式无效: %v", errDecisionMalformed, err)
		}
	}
	questions, err := questionsFromAskItems(payload.Questions)
	if err != nil {
		return Decision{}, "", err
	}
	revision, err := decisionRevision(item.Raw)
	if err != nil {
		return Decision{}, "", err
	}

	callID := ""
	if payload.Wait != nil {
		callID = payload.Wait.CallID
		if callID != "" {
			if err := validateDecisionIdentifier("wait.callId", callID); err != nil {
				return Decision{}, "", err
			}
		}
	}
	return Decision{
		NativeID:  "event:" + item.EventID,
		SessionID: sessionID,
		Kind:      decisionKindForm,
		Title:     "dsh 提问",
		Questions: questions,
		Revision:  revision,
		Blocking:  payload.Wait == nil || !payload.Wait.Timed,
		Supported: true,
	}, callID, nil
}

// unsupportedWaterfallDecision 保留未知 waterfall 的事实，但不允许远程回答。
func unsupportedWaterfallDecision(item PendingItem, sessionID string) (Decision, error) {
	revision, err := decisionRevision(item.Raw)
	if err != nil {
		return Decision{}, err
	}
	event := sanitizeDecisionText(item.Event, decisionStateReasonMaxBytes*2)
	if event == "" {
		event = "未知交互"
	}
	return Decision{
		NativeID:  "event:" + item.EventID,
		SessionID: sessionID,
		Kind:      decisionKindUnsupported,
		Title:     "dsh 交互",
		Revision:  revision,
		Blocking:  false,
		Supported: false,
		Reason:    fmt.Sprintf("不支持的 dsh 交互 %q", event),
	}, nil
}

// decisionsFromProjections 读取 projections.values.userQuestions.active。
//
// state=continued 的条目可以远程回答；open 等状态保留事实但标记不支持。
// skipCallIDs 中的条目表示 timed waterfall 已经覆盖同一问题，直接跳过。
func decisionsFromProjections(sessionID string, projections json.RawMessage, skipCallIDs map[string]struct{}) ([]Decision, error) {
	trimmed := bytes.TrimSpace(projections)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var value struct {
		Values struct {
			UserQuestions struct {
				Active []json.RawMessage `json:"active"`
			} `json:"userQuestions"`
		} `json:"values"`
	}
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return nil, fmt.Errorf("%w: 会话投影格式无效: %v", errDecisionMalformed, err)
	}

	active := value.Values.UserQuestions.Active
	decisions := make([]Decision, 0, len(active))
	seen := make(map[string]struct{}, len(active))
	for index, raw := range active {
		var entry continuedQuestionEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, fmt.Errorf("%w: 第 %d 条持续问题格式无效: %v", errDecisionMalformed, index, err)
		}
		if err := validateDecisionIdentifier("callId", entry.CallID); err != nil {
			return nil, err
		}
		if _, skipped := skipCallIDs[entry.CallID]; skipped {
			continue
		}
		if _, duplicated := seen[entry.CallID]; duplicated {
			continue
		}
		seen[entry.CallID] = struct{}{}

		questions, err := questionsFromAskItems(entry.Questions)
		if err != nil {
			return nil, err
		}
		revision, err := decisionRevision(raw)
		if err != nil {
			return nil, err
		}
		decision := Decision{
			NativeID:  "call:" + entry.CallID,
			SessionID: sessionID,
			Kind:      decisionKindForm,
			Title:     "dsh 提问",
			Questions: questions,
			Revision:  revision,
		}
		switch entry.State {
		case "continued":
			// continued 表示前台请求已返回 pending，可以安全地把回答作为新消息注入。
			decision.Supported = true
			decision.Blocking = true
		case "open":
			decision.Reason = "该问题仍在等待 dsh 原生界面回答"
		default:
			state := sanitizeDecisionText(entry.State, decisionStateReasonMaxBytes)
			if state == "" {
				state = "未知"
			}
			decision.Reason = fmt.Sprintf("持续问题状态 %q 不支持远程回答", state)
		}
		decisions = append(decisions, decision)
	}
	return decisions, nil
}

// questionsFromAskItems 校验并转换 dsh 原生问题数组。
//
// 原生选项只有 label，因此 Value 与 label 相同；展示文本会清理 ANSI/控制字符，
// 但 Value 保持原样，保证回答能精确匹配原生选项。
func questionsFromAskItems(items []askUserQuestionItem) ([]Question, error) {
	if len(items) < 1 || len(items) > decisionMaxQuestions {
		return nil, fmt.Errorf("%w: 问题数 %d 超出 1..%d", errDecisionMalformed, len(items), decisionMaxQuestions)
	}
	seen := make(map[string]struct{}, len(items))
	questions := make([]Question, 0, len(items))
	for index, item := range items {
		if err := validateDecisionIdentifier(fmt.Sprintf("第 %d 个问题 ID", index+1), item.ID); err != nil {
			return nil, err
		}
		if _, duplicated := seen[item.ID]; duplicated {
			return nil, fmt.Errorf("%w: 问题 ID %q 重复", errDecisionMalformed, item.ID)
		}
		seen[item.ID] = struct{}{}

		if len(item.Question) > decisionTextMaxBytes || len(item.Header) > decisionTextMaxBytes || len(item.Detail) > decisionTextMaxBytes {
			return nil, fmt.Errorf("%w: 第 %d 个问题的文本超过 %d 字节", errDecisionMalformed, index+1, decisionTextMaxBytes)
		}
		if len(item.Options) > decisionMaxOptions {
			return nil, fmt.Errorf("%w: 第 %d 个问题的选项数超过 %d", errDecisionMalformed, index+1, decisionMaxOptions)
		}
		options := make([]Option, 0, len(item.Options))
		for optionIndex, option := range item.Options {
			if strings.TrimSpace(option.Label) == "" {
				return nil, fmt.Errorf("%w: 第 %d 个问题第 %d 个选项 label 为空", errDecisionMalformed, index+1, optionIndex+1)
			}
			if len(option.Label) > decisionTextMaxBytes || len(option.Description) > decisionTextMaxBytes {
				return nil, fmt.Errorf("%w: 第 %d 个问题第 %d 个选项文本超过 %d 字节",
					errDecisionMalformed, index+1, optionIndex+1, decisionTextMaxBytes)
			}
			label := sanitizeDecisionText(option.Label, decisionTextMaxBytes)
			if label == "" {
				// 选项只含控制字符时仍保留可编号性，但绝不展示原始控制序列。
				label = "选项"
			}
			options = append(options, Option{
				Value:       option.Label,
				Label:       label,
				Description: sanitizeDecisionText(option.Description, decisionTextMaxBytes),
			})
		}

		kind := questionKindText
		switch {
		case len(options) == 0:
			kind = questionKindText
		case item.MultiSelect:
			kind = questionKindMultiple
		default:
			kind = questionKindSingle
		}

		title := sanitizeDecisionText(item.Question, decisionTextMaxBytes)
		if title == "" {
			title = sanitizeDecisionText(item.Header, decisionTextMaxBytes)
		}
		if title == "" {
			title = "问题 " + item.ID
		}
		questions = append(questions, Question{
			ID:          item.ID,
			Title:       title,
			Description: sanitizeDecisionText(item.Detail, decisionTextMaxBytes),
			Kind:        kind,
			Options:     options,
			Required:    true,
		})
	}
	return questions, nil
}

// approvalOptions 返回审批固定选项；只允许“本次允许”或“拒绝”。
func approvalOptions() []Option {
	return []Option{
		{Value: approvalAllowedOnce, Label: "允许本次"},
		{Value: approvalRejected, Label: "拒绝"},
	}
}

// preferredDisplayReason 按中文优先顺序选择 dsh 提供的本地化原因。
func preferredDisplayReason(reasons map[string]string) string {
	for _, key := range []string{"zh", "zh-CN", "zh-Hans", "en"} {
		if value := strings.TrimSpace(reasons[key]); value != "" {
			return value
		}
	}
	return ""
}

// decisionRevision 对原始帧做键序稳定的 JSON 归一化后计算 sha256。
func decisionRevision(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", fmt.Errorf("%w: 决策缺少原始帧", errDecisionMalformed)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%w: 决策原始帧不是有效 JSON: %v", errDecisionMalformed, err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("%w: 决策原始帧无法归一化", errDecisionMalformed)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// validateDecisionIdentifier 校验会进入回答载荷的 ID；与 Mux 标识规则保持一致。
func validateDecisionIdentifier(field, value string) error {
	if value == "" {
		return fmt.Errorf("%w: %s 为空", errDecisionMalformed, field)
	}
	if len(value) > decisionIDMaxBytes {
		return fmt.Errorf("%w: %s 超过 %d 字节", errDecisionMalformed, field, decisionIDMaxBytes)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s 不是合法 UTF-8", errDecisionMalformed, field)
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: %s 含控制字符", errDecisionMalformed, field)
	}
	return nil
}

// sanitizeDecisionText 清理 ANSI/控制字符并按 UTF-8 边界截断展示文本。
func sanitizeDecisionText(text string, maxBytes int) string {
	cleaned := strings.TrimSpace(stripTranscriptControl(text))
	if maxBytes <= 0 || len(cleaned) <= maxBytes {
		return cleaned
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(cleaned[cut]) {
		cut--
	}
	return cleaned[:cut]
}

// formatDecision 把一条决策渲染为 IM 文本，包含标题、选项编号与 /answer 提示。
//
// questionIndex 是当前待回答的问题下标；越界时回退到第一个问题。
// 多题决策会显示 (i/n) 进度，避免用户误以为一次 /answer 即可提交全部答案。
func formatDecision(decision Decision, questionIndex int) string {
	var builder strings.Builder
	title := sanitizeDecisionText(decision.Title, decisionTextMaxBytes)
	if title == "" {
		title = "dsh 交互"
	}
	builder.WriteString(title)

	if !decision.Supported {
		if decision.Reason != "" {
			builder.WriteString("\n")
			builder.WriteString(decision.Reason)
		}
		builder.WriteString("\n请在 dsh 原生界面处理。")
		return builder.String()
	}
	if len(decision.Questions) == 0 {
		return builder.String()
	}
	if questionIndex < 0 || questionIndex >= len(decision.Questions) {
		questionIndex = 0
	}

	question := decision.Questions[questionIndex]
	builder.WriteString("\n")
	if len(decision.Questions) > 1 {
		fmt.Fprintf(&builder, "(%d/%d) ", questionIndex+1, len(decision.Questions))
	}
	builder.WriteString(question.Title)
	if question.Description != "" {
		builder.WriteString("\n")
		builder.WriteString(question.Description)
	}
	for index, option := range question.Options {
		fmt.Fprintf(&builder, "\n%d. %s", index+1, option.Label)
		if option.Description != "" {
			fmt.Fprintf(&builder, "（%s）", option.Description)
		}
	}
	builder.WriteString("\n")
	builder.WriteString(answerHint(question))
	return builder.String()
}

// answerHint 根据问题类型生成 /answer 用法提示。
func answerHint(question Question) string {
	switch question.Kind {
	case questionKindApproval:
		return "回复 /answer 1 允许本次，/answer 2 拒绝。"
	case questionKindText:
		return "回复 /answer 自定义文本。"
	case questionKindMultiple:
		if len(question.Options) >= 2 {
			return "回复 /answer 1,2（多个用逗号分隔，可附加自定义文本）。"
		}
		if len(question.Options) == 1 {
			return "回复 /answer 1（可附加自定义文本）。"
		}
	case questionKindSingle:
		if len(question.Options) > 0 {
			return "回复 /answer 1（可附加自定义文本）。"
		}
	}
	return "回复 /answer 自定义文本。"
}

// Client.Decisions 读取会话投影并合并当前 Mux 待答事件。
//
// 任一来源不可用或格式错误时降级为另一个来源；两者都失败才返回错误。
// 每次调用都重新读取 session/projections，避免使用过期的持续问题状态。
func (c *Client) Decisions(ctx context.Context, sessionID string) ([]Decision, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	raw, projectionErr := c.rpc.call(ctx, "session/projections", projectionsArgs{
		Request: projectionsRequest{SessionID: sessionID},
	})
	if projectionErr != nil {
		projectionErr = fmt.Errorf("读取 dsh 会话投影失败: %w", projectionErr)
	}
	pending, pendingErr := c.Pending(ctx)
	if pendingErr != nil {
		pendingErr = fmt.Errorf("读取 dsh 待答事件失败: %w", pendingErr)
	}
	if projectionErr != nil && pendingErr != nil {
		return nil, errors.Join(projectionErr, pendingErr)
	}

	var decisions []Decision
	var callIDs map[string]struct{}
	if pendingErr == nil {
		pendingDecisions, usedCallIDs, err := decisionsFromPending(sessionID, pending)
		if err != nil {
			pendingErr = fmt.Errorf("解析 dsh 待答事件失败: %w", err)
		} else {
			decisions = append(decisions, pendingDecisions...)
			callIDs = usedCallIDs
		}
	}
	if projectionErr == nil {
		projectionDecisions, err := decisionsFromProjections(sessionID, raw, callIDs)
		if err != nil {
			projectionErr = fmt.Errorf("解析 dsh 会话投影失败: %w", err)
		} else {
			decisions = append(decisions, projectionDecisions...)
		}
	}
	if pendingErr != nil && projectionErr != nil {
		return nil, errors.Join(pendingErr, projectionErr)
	}
	if decisions == nil {
		decisions = []Decision{}
	}
	return decisions, nil
}
