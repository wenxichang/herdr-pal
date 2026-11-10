package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// approvalAllowedOnce 是“本次允许”的原生审批值。
	approvalAllowedOnce = "allowed-once"
	// approvalRejected 是“拒绝”的原生审批值。
	approvalRejected = "rejected"
	// answerCustomMaxRunes 限制自定义回答的字符数，与设计文档的 16 KiB 预算一致。
	answerCustomMaxRunes = 16384
)

var (
	// errAnswerInvalid 表示用户回答不符合问题类型或选项范围。
	errAnswerInvalid = errors.New("dsh 回答无效")
	// errDecisionUnsupported 表示该决策不允许远程回答。
	errDecisionUnsupported = errors.New("dsh 决策不支持远程回答")
	// errDecisionStale 表示决策已被替换、取消或不再属于当前待答表。
	errDecisionStale = errors.New("dsh 决策已失效")
)

// Answer 是用户对一个问题的回答。
//
// Selected 保存原生选项值；Custom 是用户附加的自由文本。审批只允许一个 Selected，
// 且不允许 Custom。
type Answer struct {
	// Selected 是按选择顺序排列的原生选项值。
	Selected []string
	// Custom 是清理首尾空白后的自由文本。
	Custom string
}

// AnswerReceipt 是提交回答后的确认结果。
//
// Confirmed=false 不代表请求一定失败：Mux 已换代导致本地无法注销待答事件，
// 或 dsh 返回未接受时，需要用户核对原生界面。
type AnswerReceipt struct {
	// Confirmed 表示是否观察到 dsh 确认。
	Confirmed bool
	// Message 是可直接展示给用户的固定提示。
	Message string
}

// answerItem 是 userQuestions/answer 中的单题答案。
type answerItem struct {
	ID       string   `json:"id"`
	Selected []string `json:"selected"`
	Custom   string   `json:"custom,omitempty"`
}

// answerValue 是 userQuestions/answer 的答案载荷。
type answerValue struct {
	Answers []answerItem `json:"answers"`
}

// parseAnswer 校验 /answer 解析出的编号与原文，并转换为原生答案。
//
// 编号为 1 基；approval 只允许一个编号且禁止自定义文本；single 最多一个编号；
// multiple 允许任意多个编号；text 不允许编号。编号越界、重复或自定义文本超过
// 16384 字符时失败。
func parseAnswer(indexes []int, text string, question Question) (Answer, error) {
	custom := strings.TrimSpace(text)
	if !utf8.ValidString(custom) {
		return Answer{}, fmt.Errorf("%w: 自定义文本不是合法 UTF-8", errAnswerInvalid)
	}
	if utf8.RuneCountInString(custom) > answerCustomMaxRunes {
		return Answer{}, fmt.Errorf("%w: 自定义文本超过 %d 字符", errAnswerInvalid, answerCustomMaxRunes)
	}

	seen := make(map[int]struct{}, len(indexes))
	selected := make([]string, 0, len(indexes))
	for _, index := range indexes {
		if index < 1 || index > len(question.Options) {
			return Answer{}, fmt.Errorf("%w: 编号 %d 超出范围 1..%d", errAnswerInvalid, index, len(question.Options))
		}
		if _, duplicated := seen[index]; duplicated {
			return Answer{}, fmt.Errorf("%w: 编号 %d 重复", errAnswerInvalid, index)
		}
		seen[index] = struct{}{}
		selected = append(selected, question.Options[index-1].Value)
	}

	switch question.Kind {
	case questionKindApproval:
		if len(selected) != 1 {
			return Answer{}, fmt.Errorf("%w: 审批必须且只能选择一个选项", errAnswerInvalid)
		}
		if custom != "" {
			return Answer{}, fmt.Errorf("%w: 审批不接受自定义文本", errAnswerInvalid)
		}
		if selected[0] != approvalAllowedOnce && selected[0] != approvalRejected {
			return Answer{}, fmt.Errorf("%w: 审批选项 %q 不在允许范围内", errAnswerInvalid, selected[0])
		}
	case questionKindSingle:
		if len(selected) > 1 {
			return Answer{}, fmt.Errorf("%w: 单选问题最多接受一个编号", errAnswerInvalid)
		}
		if len(selected) == 0 && custom == "" {
			return Answer{}, fmt.Errorf("%w: 单选问题至少需要一个编号或自定义文本", errAnswerInvalid)
		}
	case questionKindMultiple:
		if len(selected) == 0 && custom == "" {
			return Answer{}, fmt.Errorf("%w: 多选问题至少需要一个编号或自定义文本", errAnswerInvalid)
		}
	case questionKindText:
		if len(selected) > 0 {
			return Answer{}, fmt.Errorf("%w: 文本问题不接受编号", errAnswerInvalid)
		}
		if custom == "" {
			return Answer{}, fmt.Errorf("%w: 文本问题需要自定义文本", errAnswerInvalid)
		}
	default:
		return Answer{}, fmt.Errorf("%w: 未知问题类型 %q", errAnswerInvalid, question.Kind)
	}
	return Answer{Selected: selected, Custom: custom}, nil
}

// buildAnswerValue 把逐题答案转换为 dsh 原生提交值。
//
// approval 返回字符串 allowed-once/rejected；form 返回
// {"answers":[{"id","selected","custom"}]}，问题顺序与决策一致。
// 缺少答案、包含未知问题或答案不符合选项时失败。
func buildAnswerValue(decision Decision, answers map[string]Answer) (any, error) {
	if !decision.Supported {
		return nil, fmt.Errorf("%w: %s", errDecisionUnsupported, decision.NativeID)
	}
	if len(decision.Questions) == 0 {
		return nil, fmt.Errorf("%w: 决策没有可回答问题", errAnswerInvalid)
	}

	switch decision.Kind {
	case decisionKindApproval:
		if len(decision.Questions) != 1 {
			return nil, fmt.Errorf("%w: 审批决策必须恰好包含一个问题", errAnswerInvalid)
		}
		question := decision.Questions[0]
		answer, ok := answers[question.ID]
		if !ok {
			return nil, fmt.Errorf("%w: 缺少审批答案", errAnswerInvalid)
		}
		if len(answers) != 1 {
			return nil, fmt.Errorf("%w: 审批只能包含一个答案", errAnswerInvalid)
		}
		if custom := strings.TrimSpace(answer.Custom); custom != "" {
			return nil, fmt.Errorf("%w: 审批不接受自定义文本", errAnswerInvalid)
		}
		if len(answer.Selected) != 1 {
			return nil, fmt.Errorf("%w: 审批必须且只能选择一个选项", errAnswerInvalid)
		}
		value := answer.Selected[0]
		if value != approvalAllowedOnce && value != approvalRejected {
			return nil, fmt.Errorf("%w: 审批选项 %q 不在允许范围内", errAnswerInvalid, value)
		}
		return value, nil

	case decisionKindForm:
		if len(answers) != len(decision.Questions) {
			return nil, fmt.Errorf("%w: 答案数 %d 与问题数 %d 不一致", errAnswerInvalid, len(answers), len(decision.Questions))
		}
		items := make([]answerItem, 0, len(decision.Questions))
		for _, question := range decision.Questions {
			answer, ok := answers[question.ID]
			if !ok {
				return nil, fmt.Errorf("%w: 缺少问题 %q 的答案", errAnswerInvalid, question.ID)
			}
			if err := validateQuestionAnswer(question, answer); err != nil {
				return nil, err
			}
			custom := strings.TrimSpace(answer.Custom)
			selected := make([]string, 0, len(answer.Selected))
			selected = append(selected, answer.Selected...)
			items = append(items, answerItem{ID: question.ID, Selected: selected, Custom: custom})
		}
		return answerValue{Answers: items}, nil

	default:
		return nil, fmt.Errorf("%w: 未知决策类型 %q", errDecisionUnsupported, decision.Kind)
	}
}

// validateQuestionAnswer 在构造提交载荷前再次校验答案与问题选项的一致性。
func validateQuestionAnswer(question Question, answer Answer) error {
	custom := strings.TrimSpace(answer.Custom)
	if !utf8.ValidString(custom) {
		return fmt.Errorf("%w: 问题 %q 的自定义文本不是合法 UTF-8", errAnswerInvalid, question.ID)
	}
	if utf8.RuneCountInString(custom) > answerCustomMaxRunes {
		return fmt.Errorf("%w: 问题 %q 的自定义文本超过 %d 字符", errAnswerInvalid, question.ID, answerCustomMaxRunes)
	}

	validValues := make(map[string]struct{}, len(question.Options))
	for _, option := range question.Options {
		validValues[option.Value] = struct{}{}
	}
	seen := make(map[string]struct{}, len(answer.Selected))
	for _, value := range answer.Selected {
		if _, ok := validValues[value]; !ok {
			return fmt.Errorf("%w: 问题 %q 的选项 %q 不在范围内", errAnswerInvalid, question.ID, value)
		}
		if _, duplicated := seen[value]; duplicated {
			return fmt.Errorf("%w: 问题 %q 的选项 %q 重复", errAnswerInvalid, question.ID, value)
		}
		seen[value] = struct{}{}
	}

	switch question.Kind {
	case questionKindApproval:
		if len(answer.Selected) != 1 || custom != "" {
			return fmt.Errorf("%w: 问题 %q 的审批答案无效", errAnswerInvalid, question.ID)
		}
		if answer.Selected[0] != approvalAllowedOnce && answer.Selected[0] != approvalRejected {
			return fmt.Errorf("%w: 问题 %q 的审批选项 %q 不在允许范围内", errAnswerInvalid, question.ID, answer.Selected[0])
		}
	case questionKindSingle:
		if len(answer.Selected) > 1 {
			return fmt.Errorf("%w: 问题 %q 是单选题", errAnswerInvalid, question.ID)
		}
		if len(answer.Selected) == 0 && custom == "" {
			return fmt.Errorf("%w: 问题 %q 至少需要一个编号或自定义文本", errAnswerInvalid, question.ID)
		}
	case questionKindMultiple:
		if len(answer.Selected) == 0 && custom == "" {
			return fmt.Errorf("%w: 问题 %q 至少需要一个编号或自定义文本", errAnswerInvalid, question.ID)
		}
	case questionKindText:
		if len(answer.Selected) > 0 {
			return fmt.Errorf("%w: 问题 %q 是文本题", errAnswerInvalid, question.ID)
		}
		if custom == "" {
			return fmt.Errorf("%w: 问题 %q 需要自定义文本", errAnswerInvalid, question.ID)
		}
	default:
		return fmt.Errorf("%w: 问题 %q 的类型 %q 未知", errAnswerInvalid, question.ID, question.Kind)
	}
	return nil
}

// SubmitAnswer 提交一次完整回答，不自动重试。
//
// waterfall 审批/问题（NativeID 为 event:<id>）先重新读取 Mux 待答表，确认事件仍存在、
// 属于同一会话且 revision 未变化，再用当前 clientId 调用 $events/result；成功后由本地
// Mux 注销待答事件并给出 Confirmed，不依赖 dsh 发送取消。持续问题（call:<id>）先重新
// 读取 session/projections，确认同一 callId 仍可回答且 revision 未变化，再调用
// userQuestions/answer；只有 dsh 返回 true 才视为已进入原生队列。
func (c *Client) SubmitAnswer(ctx context.Context, decision Decision, value any) (AnswerReceipt, error) {
	if c == nil {
		return answerFailure("dsh 客户端不可用。"), errors.New("dsh 客户端不可用")
	}
	if !decision.Supported {
		return answerFailure("该交互不支持远程回答；请在 dsh 原生界面处理。"),
			fmt.Errorf("%w: %s", errDecisionUnsupported, decision.NativeID)
	}
	if decision.Kind != decisionKindApproval && decision.Kind != decisionKindForm {
		return answerFailure("该交互不支持远程回答；请在 dsh 原生界面处理。"),
			fmt.Errorf("%w: %s", errDecisionUnsupported, decision.NativeID)
	}
	if err := validateSessionID(decision.SessionID); err != nil {
		return answerFailure("dsh 会话无效；请刷新后重试。"), err
	}

	switch {
	case strings.HasPrefix(decision.NativeID, "event:"):
		eventID := strings.TrimPrefix(decision.NativeID, "event:")
		if err := validateMuxIdentifier("eventId", eventID); err != nil {
			return answerFailure("该决策标识无效；请刷新后重试。"), fmt.Errorf("%w: %v", errAnswerInvalid, err)
		}
		return c.submitEventDecision(ctx, decision, eventID, value)
	case strings.HasPrefix(decision.NativeID, "call:"):
		callID := strings.TrimPrefix(decision.NativeID, "call:")
		if err := validateMuxIdentifier("callId", callID); err != nil {
			return answerFailure("该决策标识无效；请刷新后重试。"), fmt.Errorf("%w: %v", errAnswerInvalid, err)
		}
		return c.submitContinuedDecision(ctx, decision, callID, value)
	default:
		return answerFailure("该决策标识无效；请刷新后重试。"),
			fmt.Errorf("%w: 决策标识 %q 无效", errAnswerInvalid, decision.NativeID)
	}
}

// submitEventDecision 处理 waterfall 审批/问题提交。
func (c *Client) submitEventDecision(ctx context.Context, decision Decision, eventID string, value any) (AnswerReceipt, error) {
	if decision.Kind == decisionKindApproval {
		if _, err := approvalAnswerValue(value); err != nil {
			return answerFailure("审批回答无效；请刷新后重试。"), err
		}
	}

	snapshot, err := c.Pending(ctx)
	if err != nil {
		return answerFailure("dsh 待答事件暂不可用；请稍后重试。"), fmt.Errorf("复核 dsh 待答事件失败: %w", err)
	}
	item, ok := pendingItem(snapshot, eventID)
	if !ok {
		return answerFailure("该决策已失效；请刷新后重试。"),
			fmt.Errorf("%w: eventId %q 不在当前待答表中", errDecisionStale, eventID)
	}
	if item.AgentID != decision.SessionID {
		return answerFailure("该决策已失效；请刷新后重试。"),
			fmt.Errorf("%w: eventId %q 属于其他会话", errDecisionStale, eventID)
	}
	if decision.Revision != "" {
		revision, revisionErr := decisionRevision(item.Raw)
		if revisionErr != nil || revision != decision.Revision {
			return answerFailure("该决策内容已变化；请刷新后重试。"),
				fmt.Errorf("%w: eventId %q 的 revision 已变化", errDecisionStale, eventID)
		}
	}

	// SubmitEventAnswer 不校验 clientId，必须使用刚刚复核的当前连接身份。
	if err := c.SubmitEventAnswer(ctx, snapshot.ClientID, eventID, value); err != nil {
		return answerFailure("提交 dsh 审批失败；请核对 dsh 原生界面。"), err
	}
	// dsh 不会向成功提交的客户端发送原生 cancel；必须由本地注销待答事件。
	if c.mux != nil && c.mux.ConfirmSubmitted(snapshot.ClientID, eventID) {
		return AnswerReceipt{Confirmed: true, Message: "已提交，dsh 已确认。"}, nil
	}
	return answerFailure("已提交，但本地无法确认；请核对 dsh 原生界面。"), nil
}

// submitContinuedDecision 处理投影中持续问题的回答。
func (c *Client) submitContinuedDecision(ctx context.Context, decision Decision, callID string, value any) (AnswerReceipt, error) {
	if decision.Kind != decisionKindForm {
		return answerFailure("该交互不支持远程回答；请在 dsh 原生界面处理。"),
			fmt.Errorf("%w: 持续问题必须是 form 决策", errDecisionUnsupported)
	}
	if value == nil {
		return answerFailure("回答内容为空；请刷新后重试。"), fmt.Errorf("%w: 回答内容为空", errAnswerInvalid)
	}
	if err := c.verifyContinuedDecision(ctx, decision, callID); err != nil {
		if errors.Is(err, errDecisionStale) {
			return answerFailure("该问题已变化或结束；请重新 /con 后重试。"), err
		}
		return answerFailure("dsh 问题状态暂不可用；请稍后重试。"), err
	}
	raw, err := c.rpc.call(ctx, "userQuestions/answer", map[string]any{
		"agentId": decision.SessionID,
		"callId":  callID,
		"answer":  value,
	})
	if err != nil {
		return answerFailure("提交 dsh 回答失败；请核对 dsh 原生界面。"), fmt.Errorf("提交 dsh 回答失败: %w", err)
	}
	var accepted bool
	if err := json.Unmarshal(raw, &accepted); err != nil {
		return answerFailure("dsh 回答结果格式无效；请核对 dsh 原生界面。"),
			fmt.Errorf("%w: 回答结果不是布尔值", errAnswerInvalid)
	}
	if accepted {
		return AnswerReceipt{Confirmed: true, Message: "已回答，dsh 已接收。"}, nil
	}
	return answerFailure("dsh 未接受该回答；请核对 dsh 原生界面。"), nil
}

// verifyContinuedDecision 在提交持续问题前重新读取会话投影。
//
// callId 可跨轮次复用，只按 callId 回答可能命中已修订或已结束的问题；必须确认同一
// callId 仍处于 continued（Supported）且 revision 与用户看到的一致，否则不提交。
// 读取或解析失败返回普通错误，状态不匹配返回 errDecisionStale。
func (c *Client) verifyContinuedDecision(ctx context.Context, decision Decision, callID string) error {
	raw, err := c.rpc.call(ctx, "session/projections", projectionsArgs{
		Request: projectionsRequest{SessionID: decision.SessionID},
	})
	if err != nil {
		return fmt.Errorf("复核 dsh 持续问题失败: %w", err)
	}
	decisions, err := decisionsFromProjections(decision.SessionID, raw, nil)
	if err != nil {
		return fmt.Errorf("复核 dsh 持续问题失败: %w", err)
	}
	nativeID := "call:" + callID
	for _, candidate := range decisions {
		if candidate.NativeID != nativeID {
			continue
		}
		if !candidate.Supported {
			return fmt.Errorf("%w: callId %q 当前状态不接受远程回答", errDecisionStale, callID)
		}
		if candidate.Revision != decision.Revision {
			return fmt.Errorf("%w: callId %q 的 revision 已变化", errDecisionStale, callID)
		}
		return nil
	}
	return fmt.Errorf("%w: callId %q 已不在持续问题列表中", errDecisionStale, callID)
}

// pendingItem 在当前待答快照中查找指定事件。
func pendingItem(snapshot PendingSnapshot, eventID string) (PendingItem, bool) {
	for _, item := range snapshot.Items {
		if item.EventID == eventID {
			return item, true
		}
	}
	return PendingItem{}, false
}

// approvalAnswerValue 确保审批只提交两个白名单值之一。
func approvalAnswerValue(value any) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%w: 审批回答必须是字符串", errAnswerInvalid)
	}
	if text != approvalAllowedOnce && text != approvalRejected {
		return "", fmt.Errorf("%w: 审批只允许 %q 或 %q", errAnswerInvalid, approvalAllowedOnce, approvalRejected)
	}
	return text, nil
}

// answerFailure 构造未确认回执；固定提示不含认证材料或终端内容。
func answerFailure(message string) AnswerReceipt {
	return AnswerReceipt{Confirmed: false, Message: message}
}
