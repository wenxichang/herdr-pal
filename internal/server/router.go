package server

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/wenxichang/herdr-pal/internal/audit"
	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/machinereg"
	"github.com/wenxichang/herdr-pal/internal/panel"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/session"
)

var ErrInvalidRouterDependency = errors.New("ConversationRouter 依赖无效")

// ErrTerminalImageUnsupported 表示目标 Pal 没有协商终端图片能力。
var ErrTerminalImageUnsupported = errors.New("目标 Pal 不支持终端图片模式")

const defaultRelayRequestTimeout = 20 * time.Second

const backgroundNotificationActivityWindow = 2 * time.Minute

const noAvailableSessionsMessage = "当前没有可用会话，可使用 /reg <机器标识> <来源地址> 注册机器；使用 /help 获取安装帮助。"

//go:embed default_help.md
var serverHelpTextTemplate string

// DefaultHelpText 返回首次创建 help.md 时使用的客户端快速上手内容。
func DefaultHelpText() string {
	return serverHelpTextTemplate
}

// WeComGateway 是 Router 回复企业微信消息所需的动态用户发送能力。
type WeComGateway interface {
	RespondMarkdown(ctx context.Context, requestID, content string) error
	SendMarkdownTo(ctx context.Context, userID, content string) error
}

// WeComImageGateway 是 Router 可选使用的企业微信图片发送能力。
type WeComImageGateway interface {
	SendImageTo(ctx context.Context, userID string, png []byte) error
}

// RelayRequester 是 Router 复核选择和执行用户输入所需的客户端能力。
type RelayRequester interface {
	Select(ctx context.Context, userID string, target hprp.Target) error
	Execute(ctx context.Context, userID string, target hprp.Target, message im.IncomingText) (RelayExecution, error)
	SupportsCapability(userID string, target hprp.Target, capability string) bool
	FetchTerminalSnapshot(ctx context.Context, userID string, target hprp.Target, mode hprp.OutputMode, maxLines int) (hprp.TerminalSnapshotResult, error)
}

// RegistrationRequester 处理已认证企微用户的机器自主注册。
type RegistrationRequester interface {
	Register(context.Context, machinereg.RegisterInput, machinereg.KeyDeliveryFunc) (machinereg.RegisterResult, error)
}

// RelayExecution 是客户端首段回复及执行期间发生的 Agent 会话替换信息。
type RelayExecution struct {
	StructuredContent *hprp.Content
	SelectedTarget    *hprp.Target
}

// SendCommandOutput 复核后续分段的稳定来源，并发送给所属企业微信用户。
func (router *ConversationRouter) SendCommandOutput(ctx context.Context, userID string, output hprp.CommandOutput) error {
	if router == nil || strings.TrimSpace(userID) == "" {
		return ErrInvalidRouterDependency
	}
	return router.executor.Submit(ctx, userID, func(taskContext context.Context) error {
		return router.sendCommandOutput(taskContext, userID, output)
	})
}

func (router *ConversationRouter) sendCommandOutput(ctx context.Context, userID string, output hprp.CommandOutput) error {
	entry, err := router.catalog.ResolveTarget(userID, output.Target)
	if err != nil {
		return err
	}
	router.activity.Touch(userID, router.now())
	return router.sendContentPush(ctx, userID, entry, output.Content, "command_output")
}

// SendNotification 根据结构化状态事件决定是否拉取并发送终端快照。
func (router *ConversationRouter) SendNotification(ctx context.Context, userID, machineID string, notification hprp.NotificationEvent) error {
	if router == nil || strings.TrimSpace(userID) == "" {
		return ErrInvalidRouterDependency
	}
	return router.executor.Submit(ctx, userID, func(taskContext context.Context) error {
		return router.sendNotification(taskContext, userID, machineID, notification)
	})
}

func (router *ConversationRouter) sendNotification(ctx context.Context, userID, machineID string, notification hprp.NotificationEvent) error {
	if notification.Target.MachineID != machineID {
		return ErrTargetChanged
	}
	recentlyActive := router.activity.RecentlyActiveAndTouch(userID, router.now(), backgroundNotificationActivityWindow)
	if notification.Kind == hprp.NotificationKindTargetInvalidated {
		return router.gateway.SendMarkdownTo(ctx, userID, invalidatedNotificationText(notification.Target))
	}
	if notification.Kind != hprp.NotificationKindAgentStatusChanged || notification.Data == nil {
		return hprp.ErrInvalidMessage
	}
	entry, err := router.catalog.ResolveTarget(userID, notification.Target)
	if err != nil {
		return err
	}
	statusText := statusNotificationText(entry, notification.Data.Status)
	if !statusNeedsTerminal(notification.Data.PreviousStatus, notification.Data.Status) {
		return router.gateway.SendMarkdownTo(ctx, userID, statusText)
	}
	selected, selectedErr := router.catalog.Selected(userID)
	if selectedErr == nil && !sameSessionRef(selected.Ref, entry.Ref) && recentlyActive {
		listIndex, err := router.catalog.EnsureNumberedIndex(userID, entry.Ref)
		if err != nil {
			return err
		}
		content := fmt.Sprintf("⚠️ %s 有新的输出，等待你的回复，使用/%d切换", catalogTargetLabel(entry), listIndex)
		return router.gateway.SendMarkdownTo(ctx, userID, content)
	}
	if err := router.gateway.SendMarkdownTo(ctx, userID, statusText); err != nil {
		return err
	}
	mode := router.effectiveOutputMode(userID, entry)
	requestContext, cancel := context.WithTimeout(ctx, router.requestTimeout)
	result, err := router.relay.FetchTerminalSnapshot(requestContext, userID, entry.Ref, mode, 100)
	cancel()
	if err != nil {
		router.logger.Warn("Agent 状态通知终端读取失败", append(append([]any{"user_hash", routerHash(userID), "status", notification.Data.Status}, targetLogArgs(entry.Ref)...), serverErrorLogArgs(err)...)...)
		return nil
	}
	content := result.Content
	if result.Outcome != hprp.OutcomeOK {
		content = result.FallbackContent
	}
	if content == nil {
		return nil
	}
	if err := router.sendContentPush(ctx, userID, entry, *content, "status_notification"); err != nil {
		router.logger.Warn("Agent 状态通知终端发送失败", append(append([]any{"user_hash", routerHash(userID), "status", notification.Data.Status}, targetLogArgs(entry.Ref)...), serverErrorLogArgs(err)...)...)
		return nil
	}
	return nil
}

// ConversationRouter 处理企业微信全局命令并把其他输入转发给选中机器。
type ConversationRouter struct {
	catalog        *SessionCatalog
	executor       *UserExecutor
	gateway        WeComGateway
	relay          RelayRequester
	deduper        *policy.Deduper
	logger         *slog.Logger
	helpProvider   HelpProvider
	requestTimeout time.Duration
	activity       *userActivityTracker
	now            func() time.Time
	rateLimiter    *UserRateLimiter
	auditor        audit.Auditor
	auditRedactor  *audit.Redactor
	botIDHash      string
	registrations  RegistrationRequester
	regApproval    RegistrationApprovalHandler
}

// ConversationRouterConfig 是服务端会话路由器的展示配置。
type ConversationRouterConfig struct {
	// HelpProvider 每次 /help 请求都读取最新内容；nil 仅用于测试和内嵌默认值。
	HelpProvider HelpProvider
	// RateLimiter 在消息去重后限制单个用户的唯一输入频率；nil 表示禁用。
	RateLimiter *UserRateLimiter
	// Auditor 接收用户输入和终端文本审计事件；nil 表示不输出。
	Auditor audit.Auditor
	// AuditRedactor 在正文离开 Router 前移除 Server 持有的凭据。
	AuditRedactor *audit.Redactor
	// BotIDHash 用于跨事件关联机器人，但不暴露 Bot ID 原文。
	BotIDHash string
	// RegistrationRequester 处理 /reg；nil 时仅返回注册服务不可用。
	RegistrationRequester RegistrationRequester
	// RegistrationApproval 处理企业微信管理员注册审批；nil 表示禁用。
	RegistrationApproval RegistrationApprovalHandler
}

// NewConversationRouter 创建多用户会话路由器。
func NewConversationRouter(catalog *SessionCatalog, executor *UserExecutor, gateway WeComGateway, relay RelayRequester, deduper *policy.Deduper, logger *slog.Logger) (*ConversationRouter, error) {
	return NewConversationRouterWithConfig(ConversationRouterConfig{}, catalog, executor, gateway, relay, deduper, logger)
}

// NewConversationRouterWithConfig 使用展示配置创建多用户会话路由器。
func NewConversationRouterWithConfig(config ConversationRouterConfig, catalog *SessionCatalog, executor *UserExecutor, gateway WeComGateway, relay RelayRequester, deduper *policy.Deduper, logger *slog.Logger) (*ConversationRouter, error) {
	if catalog == nil || executor == nil || gateway == nil || relay == nil || deduper == nil || logger == nil {
		return nil, ErrInvalidRouterDependency
	}
	if config.RateLimiter == nil {
		config.RateLimiter = NewUserRateLimiter(0, 0, time.Now)
	}
	if config.Auditor == nil {
		config.Auditor = audit.NoopAuditor{}
	}
	if config.AuditRedactor == nil {
		config.AuditRedactor = audit.NewRedactor(nil)
	}
	if config.HelpProvider == nil {
		config.HelpProvider = staticHelpProvider{content: DefaultHelpText()}
	}
	if config.RegistrationRequester == nil {
		config.RegistrationRequester = unavailableRegistrationRequester{}
	}
	if config.RegistrationApproval == nil {
		config.RegistrationApproval = unavailableRegistrationApprovalHandler{}
	}
	return &ConversationRouter{
		catalog: catalog, executor: executor, gateway: gateway, relay: relay,
		deduper: deduper, logger: logger, helpProvider: config.HelpProvider, requestTimeout: defaultRelayRequestTimeout,
		activity: newUserActivityTracker(), now: time.Now, rateLimiter: config.RateLimiter,
		auditor: config.Auditor, auditRedactor: config.AuditRedactor, botIDHash: config.BotIDHash,
		registrations: config.RegistrationRequester, regApproval: config.RegistrationApproval,
	}, nil
}

// Handle 校验并按 userid 串行处理一条企业微信单聊文本。
func (router *ConversationRouter) Handle(ctx context.Context, message im.IncomingText) {
	router.queueIncoming(ctx, message, true)
}

// Dispatch 按企业微信接收顺序提交消息，同一用户串行执行且不阻塞其他用户入队。
func (router *ConversationRouter) Dispatch(ctx context.Context, message im.IncomingText) {
	router.queueIncoming(ctx, message, false)
}

func (router *ConversationRouter) queueIncoming(ctx context.Context, message im.IncomingText, wait bool) {
	if router == nil {
		return
	}
	if message.ChatType != "single" || strings.TrimSpace(message.UserID) == "" {
		errorType := "unsupported_chat_type"
		reason := "服务端只处理企业微信单聊消息"
		if strings.TrimSpace(message.UserID) == "" {
			errorType = "missing_user_id"
			reason = "企业微信消息缺少用户标识"
		}
		router.logger.Warn("企业微信消息被服务端拒绝", "chat_type", safeLogValue(message.ChatType), "user_hash", routerHash(message.UserID), "error_type", errorType, "reason", reason)
		return
	}
	if strings.TrimSpace(message.MessageID) != "" && !router.deduper.AddIfNew(message.MessageID) {
		router.logger.Info("企业微信重复消息已忽略", "user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "reason", "消息幂等标识已处理")
		return
	}
	run := func(taskContext context.Context) error {
		router.handleIncoming(taskContext, message)
		return nil
	}
	var err error
	if wait {
		err = router.executor.Submit(ctx, message.UserID, run)
	} else {
		err = router.executor.Enqueue(ctx, message.UserID, run)
	}
	if errors.Is(err, ErrUserQueueFull) {
		router.logger.Warn("企业微信消息排队失败", append([]any{"user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID)}, serverErrorLogArgs(err)...)...)
		router.emitUserInputAuditEvent(message, "queue_full", nil)
		noticeErr := router.executor.EnqueueOverflow(ctx, message.UserID, func(taskContext context.Context) error {
			const content = "当前用户输入队列已满，请稍后重试。"
			if replyErr := router.reply(taskContext, message, content); replyErr != nil {
				if sendErr := router.sendMarkdownTo(taskContext, message.UserID, content); sendErr != nil {
					router.logger.Warn("企业微信队列过载通知发送失败", append([]any{
						"user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID),
					}, serverErrorLogArgs(sendErr)...)...)
				}
				router.logger.Warn("企业微信队列过载回调确认失败，已尝试主动通知", append([]any{
					"user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID),
				}, serverErrorLogArgs(replyErr)...)...)
			}
			return nil
		})
		if noticeErr != nil && !errors.Is(noticeErr, ErrUserQueueFull) && ctx.Err() == nil {
			router.logger.Warn("企业微信队列过载通知排队失败", append([]any{
				"user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID),
			}, serverErrorLogArgs(noticeErr)...)...)
		}
	} else if err != nil && ctx.Err() == nil {
		router.logger.Warn("用户消息执行失败", append([]any{"user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID)}, serverErrorLogArgs(err)...)...)
	}
}

func (router *ConversationRouter) handleIncoming(ctx context.Context, message im.IncomingText) {
	if strings.TrimSpace(message.MessageID) == "" {
		router.logger.Warn("企业微信消息被服务端拒绝", "user_hash", routerHash(message.UserID), "error_type", "missing_message_id", "reason", "企业微信消息缺少幂等标识")
		router.reply(ctx, message, "消息标识缺失，未执行任何操作。")
		return
	}
	decision := router.rateLimiter.Allow(message.UserID)
	router.emitUserInputAudit(message, decision)
	if !decision.Allowed {
		perSecond, perMinute := router.rateLimiter.Limits()
		retryMilliseconds := decision.RetryAfter.Milliseconds()
		if retryMilliseconds < 1 {
			retryMilliseconds = 1
		}
		router.logger.Warn("企业微信消息触发输入限速",
			"user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID),
			"window", decision.Window, "per_second", perSecond, "per_minute", perMinute,
			"retry_after_ms", retryMilliseconds,
		)
		retrySeconds := int(decision.RetryAfter / time.Second)
		if decision.RetryAfter%time.Second != 0 {
			retrySeconds++
		}
		if retrySeconds < 1 {
			retrySeconds = 1
		}
		router.reply(ctx, message, fmt.Sprintf("输入过于频繁，请在 %d 秒后重试。", retrySeconds))
		return
	}
	router.handleAuthorized(ctx, message)
}

func (router *ConversationRouter) emitUserInputAudit(message im.IncomingText, decision RateLimitDecision) {
	outcome := "accepted"
	attributes := make(map[string]string)
	if !decision.Allowed {
		outcome = "rate_limited"
		perSecond, perMinute := router.rateLimiter.Limits()
		attributes["limit.window"] = decision.Window
		attributes["limit.retry_after_ms"] = strconv.FormatInt(maxInt64(decision.RetryAfter.Milliseconds(), 1), 10)
		attributes["limit.per_second"] = strconv.Itoa(perSecond)
		attributes["limit.per_minute"] = strconv.Itoa(perMinute)
	}
	router.emitUserInputAuditEvent(message, outcome, attributes)
}

func (router *ConversationRouter) emitUserInputAuditEvent(message im.IncomingText, outcome string, attributes map[string]string) {
	actionName := "invalid"
	if action, err := parseServerAction(message.Content); err == nil {
		actionName = serverActionName(action.kind)
	}
	if attributes == nil {
		attributes = make(map[string]string)
	}
	now := router.now()
	source, hasSource := router.userInputAuditSource(message.UserID, message.Content)
	eventInput := audit.Event{
		EventName: audit.EventNameUserInput, Timestamp: now, PrincipalID: message.UserID,
		BotIDHash: router.botIDHash, MessageID: message.MessageID, RequestID: message.RequestID,
		Action: actionName, Outcome: outcome, Body: router.auditRedactor.Redact(message.Content), Attributes: attributes,
	}
	if hasSource {
		eventInput.MachineID = source.Ref.MachineID
		eventInput.Agent = auditAgent(source)
		eventInput.PaneID = source.Ref.SlotID
		eventInput.SessionID = source.Ref.SessionID
	}
	event, err := audit.PrepareEvent(eventInput, now, nil)
	if err != nil {
		router.logger.Warn("用户输入审计事件构造失败", "error_type", "event_id_generation")
		return
	}
	router.auditor.Emit(event)
}

func (router *ConversationRouter) userInputAuditSource(userID, content string) (CatalogEntry, bool) {
	action, err := parseServerAction(content)
	if err != nil {
		return CatalogEntry{}, false
	}
	switch action.kind {
	case serverActionSelect, serverActionDirected:
		entry, err := router.catalog.ResolveNumbered(userID, action.index)
		return entry, err == nil
	case serverActionForward, serverActionMode:
		entry, err := router.catalog.Selected(userID)
		return entry, err == nil
	default:
		return CatalogEntry{}, false
	}
}

func auditAgent(source CatalogEntry) string {
	agent := strings.TrimSpace(source.Session.Display.Agent)
	if agent == "" {
		agent = strings.TrimSpace(source.Session.Display.DisplayAgent)
	}
	return strings.ToLower(agent)
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func (router *ConversationRouter) handleAuthorized(ctx context.Context, message im.IncomingText) {
	approvalCommand := registrationApprovalCommand(message.Content)
	if approvalCommand != "" && !router.regApproval.IsAdmin(message.UserID) {
		router.logger.Warn("企业微信注册审批命令被拒绝",
			"admin_hash", routerHash(message.UserID),
			"message_hash", routerHash(message.MessageID),
			"command", approvalCommand,
			"error_type", "unauthorized",
		)
		router.reply(ctx, message, ErrRegistrationApprovalUnauthorized.Error())
		return
	}
	action, err := parseServerAction(message.Content)
	if err != nil {
		router.logger.Warn("企业微信交互解析失败", "user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "content_bytes", len([]byte(message.Content)), "error_type", "invalid_action", "reason", safeServerErrorReason(err))
		if approvalCommand == "/apr" || approvalCommand == "/rej" {
			if invalidateErr := router.regApproval.Invalidate(message.UserID); invalidateErr != nil {
				router.logger.Warn("注册审批列表快照失效失败",
					"admin_hash", routerHash(message.UserID),
					"message_hash", routerHash(message.MessageID),
					"command", approvalCommand,
					"error_type", registrationApprovalErrorType(invalidateErr),
				)
			}
			router.reply(ctx, message, err.Error()+"\n\n"+registrationApprovalSnapshotReminder)
			return
		}
		if isRegistrationCommand(message.Content) || isRegistrationApprovalCommand(message.Content) {
			router.reply(ctx, message, err.Error())
			return
		}
		if !router.catalog.HasSessions(message.UserID) {
			router.reply(ctx, message, noAvailableSessionsMessage)
			return
		}
		router.reply(ctx, message, err.Error())
		return
	}
	router.logger.Debug("企业微信交互已接收", "user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "action", serverActionName(action.kind), "target_index", action.index, "switch_after", action.switchAfter, "content_bytes", len([]byte(message.Content)))
	router.activity.Touch(message.UserID, router.now())
	if action.kind != serverActionHelp && action.kind != serverActionRegister && !isRegistrationApprovalAction(action.kind) && !router.catalog.HasSessions(message.UserID) {
		router.logger.Info("企业微信交互未路由", "user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "action", serverActionName(action.kind), "error_type", "no_sessions", "reason", "当前用户没有在线且可用的 Relay 会话")
		router.reply(ctx, message, noAvailableSessionsMessage)
		return
	}
	switch action.kind {
	case serverActionList:
		router.handleList(ctx, message)
	case serverActionSelect:
		router.handleSelect(ctx, message, action.index)
	case serverActionHelp:
		helpText, err := router.helpProvider.Read()
		if err != nil {
			router.logger.Warn("企业微信帮助内容读取失败", "user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "error_type", "help_unavailable", "reason", safeServerErrorReason(err))
			router.reply(ctx, message, "帮助内容暂时不可用，请联系管理员检查服务端 help.md。")
			return
		}
		router.reply(ctx, message, helpText)
	case serverActionRegister:
		router.handleRegistration(ctx, message, action)
	case serverActionListRegistrations, serverActionApproveRegistrations, serverActionRejectRegistrations:
		router.handleRegistrationApproval(ctx, message, action)
	case serverActionMode:
		router.handleMode(ctx, message, action.mode)
	case serverActionDirected:
		if action.mode != "" {
			router.handleDirectedMode(ctx, message, action)
		} else {
			router.handleDirectedExecute(ctx, message, action)
		}
	default:
		router.handleExecute(ctx, message)
	}
}

func (router *ConversationRouter) handleRegistration(ctx context.Context, message im.IncomingText, action serverAction) {
	result, err := router.registrations.Register(ctx, machinereg.RegisterInput{
		PrincipalID: message.UserID,
		MachineID:   action.machineID,
		Sources:     append([]string(nil), action.sources...),
	}, func(deliveryContext context.Context, delivery machinereg.KeyDelivery) error {
		content := fmt.Sprintf("机器注册成功：%s\n机器 Key（仅显示一次）：\n`%s`\n请妥善保存，并使用 /help 查看 Herdr 与 Herdr Pal 的安装步骤。",
			safeRouterLabel(delivery.MachineID), delivery.Token)
		return router.gateway.RespondMarkdown(deliveryContext, message.RequestID, content)
	})
	if err != nil {
		router.logger.Warn("企业微信机器注册失败",
			"user_hash", routerHash(message.UserID),
			"message_hash", routerHash(message.MessageID),
			"machine_id", safeLogValue(action.machineID),
			"error_type", registrationErrorType(err),
		)
		if errors.Is(err, machinereg.ErrDeliveryFailed) || errors.Is(err, machinereg.ErrRollbackFailed) {
			// Key 交付回调已经尝试占用当前响应，不能再次回复同一企业微信请求。
			return
		}
		switch {
		case errors.Is(err, machinereg.ErrMachineExists):
			router.reply(ctx, message, "该机器已经注册，不能重复签发 Key。")
		case errors.Is(err, machinereg.ErrInvalidRequest):
			router.reply(ctx, message, "/reg 用法: /reg <机器标识> <来源地址1,来源地址2>")
		default:
			router.reply(ctx, message, "机器注册失败，请稍后重试或联系管理员。")
		}
		return
	}
	switch result.Disposition {
	case machinereg.DispositionAutoIssued:
		return
	case machinereg.DispositionPending:
		if result.Request == nil {
			router.reply(ctx, message, "机器注册状态异常，请联系管理员。")
			return
		}
		router.reply(ctx, message, fmt.Sprintf("机器注册申请已提交，等待管理员审批。\n申请编号：%s\n审批通过后会通过企业微信发送机器 Key。", safeRouterLabel(result.Request.RegistrationID)))
		if err := router.regApproval.NotifyPending(ctx, *result.Request); err != nil {
			router.logger.Warn("机器注册审批通知未送达",
				"user_hash", routerHash(message.UserID),
				"message_hash", routerHash(message.MessageID),
				"machine_id", safeLogValue(result.Request.MachineID),
				"registration_id", safeLogValue(result.Request.RegistrationID),
				"error_type", serverErrorType(err),
				"reason", safeServerErrorReason(err),
			)
		}
	case machinereg.DispositionAlreadyPending:
		if result.Request == nil {
			router.reply(ctx, message, "机器注册状态异常，请联系管理员。")
			return
		}
		router.reply(ctx, message, fmt.Sprintf("该机器已有待审批申请。\n申请编号：%s\n请等待管理员处理。", safeRouterLabel(result.Request.RegistrationID)))
	default:
		router.reply(ctx, message, "机器注册状态异常，请联系管理员。")
	}
}

func (router *ConversationRouter) handleRegistrationApproval(ctx context.Context, message im.IncomingText, action serverAction) {
	if !router.regApproval.IsAdmin(message.UserID) {
		router.logger.Warn("企业微信注册审批命令被拒绝",
			"admin_hash", routerHash(message.UserID),
			"message_hash", routerHash(message.MessageID),
			"action", serverActionName(action.kind),
			"error_type", "unauthorized",
		)
		router.reply(ctx, message, ErrRegistrationApprovalUnauthorized.Error())
		return
	}
	if action.kind == serverActionListRegistrations {
		router.handleRegistrationApprovalList(ctx, message)
		return
	}

	var (
		content string
		err     error
	)
	switch action.kind {
	case serverActionApproveRegistrations:
		content, err = router.regApproval.Approve(ctx, message.UserID, action.indexes)
	case serverActionRejectRegistrations:
		content, err = router.regApproval.Reject(ctx, message.UserID, action.indexes)
	default:
		err = ErrRegistrationApprovalInvalidIndexes
	}
	if err != nil {
		router.logRegistrationApprovalCommandFailure(message, action.kind, len(action.indexes), err)
		content = registrationApprovalErrorMessage(action.kind, err)
	}
	router.reply(ctx, message, content)
}

func (router *ConversationRouter) handleRegistrationApprovalList(ctx context.Context, message im.IncomingText) {
	list, err := router.regApproval.PrepareList(message.UserID)
	if err != nil {
		router.logRegistrationApprovalCommandFailure(message, serverActionListRegistrations, 0, err)
		router.reply(ctx, message, registrationApprovalErrorMessage(serverActionListRegistrations, err))
		return
	}
	if err := router.regApproval.Invalidate(message.UserID); err != nil {
		router.logRegistrationApprovalCommandFailure(message, serverActionListRegistrations, 0, err)
		router.reply(ctx, message, registrationApprovalErrorMessage(serverActionListRegistrations, err))
		return
	}
	if err := router.reply(ctx, message, list.content); err != nil {
		router.logger.Warn("企业微信注册审批列表发送失败，快照未提交",
			"admin_hash", routerHash(message.UserID),
			"message_hash", routerHash(message.MessageID),
			"action", serverActionName(serverActionListRegistrations),
			"error_type", "delivery_failed",
			"reason", safeServerErrorReason(err),
		)
		return
	}
	if err := router.regApproval.CommitList(message.UserID, list); err != nil {
		router.logRegistrationApprovalCommandFailure(message, serverActionListRegistrations, 0, err)
		router.sendMarkdownTo(ctx, message.UserID, "注册审批列表快照保存失败，请重新执行 /ls-reg。")
	}
}

func (router *ConversationRouter) logRegistrationApprovalCommandFailure(message im.IncomingText, kind serverActionKind, selectedCount int, err error) {
	router.logger.Warn("企业微信注册审批命令执行失败",
		"admin_hash", routerHash(message.UserID),
		"message_hash", routerHash(message.MessageID),
		"action", serverActionName(kind),
		"selected_count", selectedCount,
		"error_type", registrationApprovalErrorType(err),
		"reason", safeServerErrorReason(err),
	)
}

func (router *ConversationRouter) handleList(ctx context.Context, message im.IncomingText) {
	entries := router.catalog.CreateNumberedSnapshot(message.UserID)
	if len(entries) == 0 {
		router.logger.Info("企业微信会话列表为空", "user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "action", "list", "error_type", "no_sessions", "reason", "当前用户没有在线且可选择的 Agent")
		router.reply(ctx, message, "当前没有在线且可选择的 Agent。")
		return
	}
	selected, selectedErr := router.catalog.Selected(message.UserID)
	var content strings.Builder
	content.WriteString("可选择的 Agent：")
	for index, entry := range entries {
		marker := ""
		if selectedErr == nil && sameSessionRef(selected.Ref, entry.Ref) {
			marker = "（当前选择）"
		}
		name := entry.Session.Display.DisplayAgent
		if name == "" {
			name = entry.Session.Display.Agent
		}
		fmt.Fprintf(&content, "\n%d. [%s/%d] %s", index+1, safeRouterLabel(entry.Ref.MachineID), entry.Session.Display.Index, safeRouterLabel(name))
		if entry.Session.Display.Title != "" {
			fmt.Fprintf(&content, " — %s", safeRouterLabel(entry.Session.Display.Title))
		}
		content.WriteString(marker)
		fmt.Fprintf(&content, "\n   工作区：%s\n   状态：%s", safeRouterLabel(panel.WorkspaceLabel(entry.Session.Display.Workspace, entry.Session.Display.Tab)), safeRouterLabel(panel.AgentStatusLabelValue(hprp.NormalizeStatus(entry.Session.Status))))
	}
	content.WriteString("\n使用 /N 选择目标。")
	router.logger.Debug("企业微信会话列表已生成", "user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "action", "list", "session_count", len(entries), "has_selection", selectedErr == nil)
	router.reply(ctx, message, content.String())
}

func (router *ConversationRouter) handleSelect(ctx context.Context, message im.IncomingText, index int) {
	entry, err := router.catalog.ResolveNumbered(message.UserID, index)
	if err != nil {
		router.logInteractionError(message, "select", "resolve_numbered", hprp.Target{}, err)
		router.reply(ctx, message, safeRouterError(err))
		return
	}
	requestContext, cancel := context.WithTimeout(ctx, router.requestTimeout)
	err = router.relay.Select(requestContext, message.UserID, entry.Ref)
	cancel()
	if err != nil {
		router.logInteractionError(message, "select", "relay_select", entry.Ref, err)
		router.reply(ctx, message, safeRouterError(err))
		return
	}
	if err := router.catalog.SetSelection(message.UserID, entry.Ref); err != nil {
		router.logInteractionError(message, "select", "set_selection", entry.Ref, err)
		router.reply(ctx, message, safeRouterError(err))
		return
	}
	consoleMessage := message
	consoleMessage.Content = "/con"
	consoleMessage.OutputMode = im.OutputMode(router.effectiveOutputMode(message.UserID, entry))
	requestContext, cancel = context.WithTimeout(ctx, router.requestTimeout)
	result, err := router.relay.Execute(requestContext, message.UserID, entry.Ref, consoleMessage)
	if err != nil {
		cancel()
		router.logInteractionError(message, "select", "read_console", entry.Ref, err)
		router.reply(ctx, message, "已选择 "+catalogTargetLabel(entry)+"，但"+safeRouterError(err))
		return
	}
	entry, err = router.rebindSelectedExecution(requestContext, message.UserID, entry, result)
	cancel()
	if err != nil {
		router.logInteractionError(message, "select", "rebind_selection", entry.Ref, err)
		router.reply(ctx, message, "已选择目标，但"+safeRouterError(err))
		return
	}
	router.logInteractionSuccess(message, "select", entry.Ref)
	if result.StructuredContent == nil {
		router.reply(ctx, message, "已选择 "+catalogTargetLabel(entry)+"。")
		return
	}
	if err := router.sendContentReply(ctx, message, entry, *result.StructuredContent, "select_console"); err != nil {
		router.logInteractionError(message, "select", "send_console", entry.Ref, err)
		router.reply(ctx, message, "已选择 "+catalogTargetLabel(entry)+"，但"+safeRouterError(err))
	}
}

func (router *ConversationRouter) handleExecute(ctx context.Context, message im.IncomingText) {
	entry, err := router.catalog.Selected(message.UserID)
	if err != nil {
		router.logInteractionError(message, "execute", "resolve_selection", hprp.Target{}, err)
		router.reply(ctx, message, "尚未选择 Agent，请先执行 /ls 和 /N。")
		return
	}
	message.OutputMode = im.OutputMode(router.effectiveOutputMode(message.UserID, entry))
	requestContext, cancel := context.WithTimeout(ctx, router.requestTimeout)
	result, err := router.relay.Execute(requestContext, message.UserID, entry.Ref, message)
	if err != nil {
		cancel()
		router.logInteractionError(message, "execute", "relay_execute", entry.Ref, err)
		if errors.Is(err, context.DeadlineExceeded) {
			router.reply(ctx, message, "操作可能已经提交，请先检查目标会话；服务端不会自动重试。")
			return
		}
		router.reply(ctx, message, safeRouterError(err))
		return
	}
	entry, err = router.rebindSelectedExecution(requestContext, message.UserID, entry, result)
	cancel()
	if err != nil {
		router.logInteractionError(message, "execute", "rebind_selection", entry.Ref, err)
		router.reply(ctx, message, safeRouterError(err))
		return
	}
	router.logInteractionSuccess(message, "execute", entry.Ref)
	if result.StructuredContent == nil {
		router.reply(ctx, message, "客户端已处理。")
		return
	}
	if err := router.sendContentReply(ctx, message, entry, *result.StructuredContent, "command_result"); err != nil {
		router.logInteractionError(message, "execute", "send_response", entry.Ref, err)
		router.reply(ctx, message, safeRouterError(err))
	}
}

func (router *ConversationRouter) handleDirectedExecute(ctx context.Context, message im.IncomingText, action serverAction) {
	entry, err := router.catalog.ResolveNumbered(message.UserID, action.index)
	if err != nil {
		router.logInteractionError(message, "directed_execute", "resolve_numbered", hprp.Target{}, err)
		router.reply(ctx, message, safeRouterError(err))
		return
	}
	directedMessage := message
	directedMessage.Content = action.content
	directedMessage.OutputMode = im.OutputMode(router.effectiveOutputMode(message.UserID, entry))
	selectedBefore, selectedErr := router.catalog.Selected(message.UserID)
	requestContext, cancel := context.WithTimeout(ctx, router.requestTimeout)
	result, err := router.relay.Execute(requestContext, message.UserID, entry.Ref, directedMessage)
	if err != nil {
		cancel()
		router.logInteractionError(message, "directed_execute", "relay_execute", entry.Ref, err)
		if errors.Is(err, context.DeadlineExceeded) {
			router.reply(ctx, message, "操作可能已经提交，但未切换当前会话；请先检查目标会话。")
			return
		}
		router.reply(ctx, message, safeRouterError(err))
		return
	}
	finalEntry := entry
	if result.SelectedTarget != nil {
		replacement := *result.SelectedTarget
		if action.switchAfter {
			if err := router.catalog.SetSelectionWhenAvailable(requestContext, message.UserID, replacement); err != nil {
				cancel()
				router.logInteractionError(message, "directed_execute", "set_replacement_selection", replacement, err)
				router.reply(ctx, message, "操作已执行，但切换当前会话失败："+safeRouterError(err))
				return
			}
		} else if selectedErr == nil && sameSessionRef(selectedBefore.Ref, entry.Ref) {
			if err := router.catalog.RebindSelection(requestContext, message.UserID, entry.Ref, replacement); err != nil {
				cancel()
				router.logInteractionError(message, "directed_execute", "rebind_current_selection", replacement, err)
				router.reply(ctx, message, "操作已执行，但当前会话更新失败："+safeRouterError(err))
				return
			}
		}
		finalEntry, err = router.catalog.WaitForTarget(requestContext, message.UserID, replacement)
		if err != nil {
			cancel()
			router.logInteractionError(message, "directed_execute", "wait_replacement_target", replacement, err)
			router.reply(ctx, message, "操作已执行，但目标会话更新失败："+safeRouterError(err))
			return
		}
	} else if action.switchAfter {
		if err := router.catalog.SetSelection(message.UserID, entry.Ref); err != nil {
			cancel()
			router.logInteractionError(message, "directed_execute", "set_selection", entry.Ref, err)
			router.reply(ctx, message, "操作已执行，但切换当前会话失败："+safeRouterError(err))
			return
		}
	}
	cancel()
	router.logInteractionSuccess(message, "directed_execute", finalEntry.Ref)
	if result.StructuredContent == nil {
		router.reply(ctx, message, "客户端已处理。")
		return
	}
	if err := router.sendContentReply(ctx, message, finalEntry, *result.StructuredContent, "command_result"); err != nil {
		router.logInteractionError(message, "directed_execute", "send_response", finalEntry.Ref, err)
		router.reply(ctx, message, safeRouterError(err))
	}
}

func (router *ConversationRouter) handleMode(ctx context.Context, message im.IncomingText, mode hprp.OutputMode) {
	entry, err := router.catalog.Selected(message.UserID)
	if err != nil {
		router.logInteractionError(message, "mode", "resolve_selection", hprp.Target{}, err)
		router.reply(ctx, message, "尚未选择 Agent，请先执行 /ls 和 /N。")
		return
	}
	if err := router.setOutputMode(message.UserID, entry, mode); err != nil {
		router.logInteractionError(message, "mode", "set_mode", entry.Ref, err)
		router.reply(ctx, message, safeRouterError(err))
		return
	}
	router.logInteractionSuccess(message, "mode", entry.Ref)
	router.reply(ctx, message, modeConfirmation(entry, mode))
}

func (router *ConversationRouter) handleDirectedMode(ctx context.Context, message im.IncomingText, action serverAction) {
	entry, err := router.catalog.ResolveNumbered(message.UserID, action.index)
	if err != nil {
		router.logInteractionError(message, "directed_mode", "resolve_numbered", hprp.Target{}, err)
		router.reply(ctx, message, safeRouterError(err))
		return
	}
	if err := router.setOutputMode(message.UserID, entry, action.mode); err != nil {
		router.logInteractionError(message, "directed_mode", "set_mode", entry.Ref, err)
		router.reply(ctx, message, safeRouterError(err))
		return
	}
	if action.switchAfter {
		if err := router.catalog.SetSelection(message.UserID, entry.Ref); err != nil {
			router.logInteractionError(message, "directed_mode", "set_selection", entry.Ref, err)
			router.reply(ctx, message, "模式已设置，但切换当前会话失败："+safeRouterError(err))
			return
		}
	}
	router.logInteractionSuccess(message, "directed_mode", entry.Ref)
	router.reply(ctx, message, modeConfirmation(entry, action.mode))
}

func (router *ConversationRouter) setOutputMode(userID string, entry CatalogEntry, mode hprp.OutputMode) error {
	if mode == hprp.OutputModeImage && !router.relay.SupportsCapability(userID, entry.Ref, hprp.CapabilityTerminalImageV1) {
		return ErrTerminalImageUnsupported
	}
	return router.catalog.SetOutputMode(userID, entry.Ref, mode)
}

func (router *ConversationRouter) effectiveOutputMode(userID string, entry CatalogEntry) hprp.OutputMode {
	if mode, explicit, err := router.catalog.OutputMode(userID, entry.Ref); err == nil && explicit {
		if mode != hprp.OutputModeImage || router.relay.SupportsCapability(userID, entry.Ref, hprp.CapabilityTerminalImageV1) {
			return mode
		}
	}
	agent := strings.TrimSpace(entry.Session.Display.Agent)
	displayAgent := strings.TrimSpace(entry.Session.Display.DisplayAgent)
	if (strings.EqualFold(agent, "opencode") || strings.EqualFold(displayAgent, "opencode")) &&
		router.relay.SupportsCapability(userID, entry.Ref, hprp.CapabilityTerminalImageV1) {
		return hprp.OutputModeImage
	}
	return hprp.OutputModeText
}

func (router *ConversationRouter) rebindSelectedExecution(ctx context.Context, userID string, source CatalogEntry, result RelayExecution) (CatalogEntry, error) {
	if result.SelectedTarget == nil {
		return source, nil
	}
	replacement := *result.SelectedTarget
	if err := router.catalog.RebindSelection(ctx, userID, source.Ref, replacement); err != nil {
		return CatalogEntry{}, err
	}
	return router.catalog.ResolveTarget(userID, replacement)
}

func (router *ConversationRouter) sendContentReply(ctx context.Context, message im.IncomingText, source CatalogEntry, content hprp.Content, action string) error {
	if content.Type == hprp.ContentTypeText {
		if strings.TrimSpace(content.Text) == "" {
			return router.reply(ctx, message, "客户端已处理。")
		} else {
			return router.reply(ctx, message, content.Text)
		}
	}
	if content.Type != hprp.ContentTypeTerminal {
		return hprp.ErrInvalidMessage
	}
	presentation := hprp.OutputModeText
	var sendErr error
	if content.Mode == hprp.OutputModeImage && content.Image != nil {
		presentation, sendErr = router.sendTerminalImageReplyResult(ctx, message, source, content)
	} else {
		var text string
		text, sendErr = router.renderTerminalText(message.UserID, source, content)
		if sendErr == nil {
			sendErr = router.reply(ctx, message, text)
		}
	}
	router.emitTerminalAudit(message.UserID, message.MessageID, message.RequestID, source, content, action, "reply", presentation, sendErr)
	return sendErr
}

func (router *ConversationRouter) sendContentPush(ctx context.Context, userID string, source CatalogEntry, content hprp.Content, action string) error {
	if content.Type == hprp.ContentTypeText {
		return router.sendMarkdownTo(ctx, userID, content.Text)
	}
	if content.Type != hprp.ContentTypeTerminal {
		return hprp.ErrInvalidMessage
	}
	presentation := hprp.OutputModeText
	var sendErr error
	if content.Mode == hprp.OutputModeImage && content.Image != nil {
		presentation, sendErr = router.sendTerminalImagePushResult(ctx, userID, source, content)
	} else {
		var text string
		text, sendErr = router.renderTerminalText(userID, source, content)
		if sendErr == nil {
			sendErr = router.sendMarkdownTo(ctx, userID, text)
		}
	}
	router.emitTerminalAudit(userID, "", "", source, content, action, "push", presentation, sendErr)
	return sendErr
}

func (router *ConversationRouter) sendTerminalImageReply(ctx context.Context, message im.IncomingText, source CatalogEntry, content hprp.Content) error {
	_, err := router.sendTerminalImageReplyResult(ctx, message, source, content)
	return err
}

func (router *ConversationRouter) sendTerminalImageReplyResult(ctx context.Context, message im.IncomingText, source CatalogEntry, content hprp.Content) (hprp.OutputMode, error) {
	imageGateway, ok := router.gateway.(WeComImageGateway)
	if !ok {
		return hprp.OutputModeText, router.sendTerminalTextFallbackReply(ctx, message, source, content)
	}
	png, err := decodeTerminalPNG(content)
	if err != nil {
		return hprp.OutputModeText, router.sendTerminalTextFallbackReply(ctx, message, source, content)
	}
	header, err := router.terminalHeader(message.UserID, source, content)
	if err != nil {
		return hprp.OutputModeImage, err
	}
	if err := router.reply(ctx, message, header); err != nil {
		return hprp.OutputModeImage, err
	}
	if err := imageGateway.SendImageTo(ctx, message.UserID, png); err != nil {
		router.logger.Warn("企业微信终端图片发送失败，降级为文本", append(append([]any{"user_hash", routerHash(message.UserID)}, targetLogArgs(source.Ref)...), serverErrorLogArgs(err)...)...)
		text, renderErr := router.renderTerminalText(message.UserID, source, content)
		if renderErr != nil {
			return hprp.OutputModeText, renderErr
		}
		return hprp.OutputModeText, router.sendMarkdownTo(ctx, message.UserID, text)
	}
	return hprp.OutputModeImage, nil
}

func (router *ConversationRouter) sendTerminalTextFallbackReply(ctx context.Context, message im.IncomingText, source CatalogEntry, content hprp.Content) error {
	text, err := router.renderTerminalText(message.UserID, source, content)
	if err != nil {
		return err
	}
	return router.reply(ctx, message, text)
}

func (router *ConversationRouter) sendTerminalImagePush(ctx context.Context, userID string, source CatalogEntry, content hprp.Content) error {
	_, err := router.sendTerminalImagePushResult(ctx, userID, source, content)
	return err
}

func (router *ConversationRouter) sendTerminalImagePushResult(ctx context.Context, userID string, source CatalogEntry, content hprp.Content) (hprp.OutputMode, error) {
	imageGateway, ok := router.gateway.(WeComImageGateway)
	if !ok {
		return hprp.OutputModeText, router.sendTerminalTextFallbackPush(ctx, userID, source, content)
	}
	png, err := decodeTerminalPNG(content)
	if err != nil {
		return hprp.OutputModeText, router.sendTerminalTextFallbackPush(ctx, userID, source, content)
	}
	header, err := router.terminalHeader(userID, source, content)
	if err != nil {
		return hprp.OutputModeImage, err
	}
	if err := router.sendMarkdownTo(ctx, userID, header); err != nil {
		return hprp.OutputModeImage, err
	}
	if err := imageGateway.SendImageTo(ctx, userID, png); err != nil {
		router.logger.Warn("企业微信终端图片发送失败，降级为文本", append(append([]any{"user_hash", routerHash(userID)}, targetLogArgs(source.Ref)...), serverErrorLogArgs(err)...)...)
		return hprp.OutputModeText, router.sendTerminalTextFallbackPush(ctx, userID, source, content)
	}
	return hprp.OutputModeImage, nil
}

func (router *ConversationRouter) sendTerminalTextFallbackPush(ctx context.Context, userID string, source CatalogEntry, content hprp.Content) error {
	text, err := router.renderTerminalText(userID, source, content)
	if err != nil {
		return err
	}
	return router.sendMarkdownTo(ctx, userID, text)
}

func (router *ConversationRouter) emitTerminalAudit(userID, messageID, requestID string, source CatalogEntry, content hprp.Content, action, delivery string, presentation hprp.OutputMode, sendErr error) {
	outcome := "delivered"
	if sendErr != nil {
		outcome = "delivery_failed"
	}
	requestedPresentation := content.Mode
	if requestedPresentation == "" {
		requestedPresentation = hprp.OutputModeText
	}
	attributes := map[string]string{"requested_presentation": string(requestedPresentation)}
	if content.Page != nil {
		attributes["page.current"] = strconv.Itoa(content.Page.Current)
		attributes["page.total"] = strconv.Itoa(content.Page.Total)
	}
	timestamp := router.now()
	if content.CapturedAt != nil && !content.CapturedAt.IsZero() {
		timestamp = *content.CapturedAt
	}
	observedAt := router.now()
	event, err := audit.PrepareEvent(audit.Event{
		EventName: audit.EventNameTerminalOutput, Timestamp: timestamp, PrincipalID: userID,
		BotIDHash: router.botIDHash, MessageID: messageID, RequestID: requestID,
		Action: action, Outcome: outcome, MachineID: source.Ref.MachineID, Agent: auditAgent(source), PaneID: source.Ref.SlotID,
		SessionID: source.Ref.SessionID, Presentation: string(presentation), Delivery: delivery,
		Body: router.auditRedactor.Redact(content.Text), Attributes: attributes,
	}, observedAt, nil)
	if err != nil {
		router.logger.Warn("终端输出审计事件构造失败", "error_type", "event_id_generation")
		return
	}
	router.auditor.Emit(event)
}

func (router *ConversationRouter) renderTerminalText(userID string, source CatalogEntry, content hprp.Content) (string, error) {
	current, total := terminalPageNumbers(content.Page)
	target := session.Target{
		PaneID: source.Session.SlotID, Agent: source.Session.Display.Agent,
		DisplayAgent: source.Session.Display.DisplayAgent, Workspace: source.Session.Display.Workspace,
		Tab: source.Session.Display.Tab, Title: source.Session.Display.Title,
	}
	rendered := panel.RenderPageWithTotal(target, current, total, strings.Split(content.Text, "\n"))
	return router.decorateTerminalContent(userID, source, rendered)
}

func (router *ConversationRouter) terminalHeader(userID string, source CatalogEntry, content hprp.Content) (string, error) {
	listIndex, err := router.catalog.EnsureNumberedIndex(userID, source.Ref)
	if err != nil {
		return "", err
	}
	current, total := terminalPageNumbers(content.Page)
	header := fmt.Sprintf("[终端输出#%d] %s, 页码:[%d/%d]", listIndex, catalogTargetLabel(source), current, total)
	selected, selectedErr := router.catalog.Selected(userID)
	if selectedErr == nil && !sameSessionRef(selected.Ref, source.Ref) {
		header += fmt.Sprintf("\n⚠️⚠️⚠️ 你的输入不会发送到该输出会话，使用 /%d 切换到当前输出的会话。", listIndex)
	}
	return header, nil
}

func terminalPageNumbers(page *hprp.TerminalPage) (int, int) {
	if page == nil {
		return 1, 1
	}
	current := max(1, page.Current)
	return current, max(current, page.Total)
}

func decodeTerminalPNG(content hprp.Content) ([]byte, error) {
	if content.Image == nil || content.Image.MediaType != "image/png" || content.Image.Encoding != "base64" {
		return nil, hprp.ErrInvalidMessage
	}
	png, err := base64.StdEncoding.DecodeString(content.Image.Data)
	if err != nil || len(png) == 0 {
		return nil, hprp.ErrInvalidMessage
	}
	return png, nil
}

func (router *ConversationRouter) sendMarkdownTo(ctx context.Context, userID, content string) error {
	parts := panel.SplitMarkdown(content, panel.WeComContentLimit)
	if len(parts) == 0 {
		return errors.New("发送内容无效")
	}
	for _, part := range parts {
		if err := router.gateway.SendMarkdownTo(ctx, userID, part); err != nil {
			return err
		}
	}
	return nil
}

func (router *ConversationRouter) decorateTerminalContent(userID string, source CatalogEntry, content string) (string, error) {
	if !panel.IsRenderedPage(content) {
		return content, nil
	}
	listIndex, err := router.catalog.EnsureNumberedIndex(userID, source.Ref)
	if err != nil {
		return "", err
	}
	content = panel.DecorateRenderedPage(content, source.Ref.MachineID, source.Session.Display.Index, listIndex)
	selected, selectedErr := router.catalog.Selected(userID)
	if selectedErr == nil && !sameSessionRef(selected.Ref, source.Ref) {
		warning := fmt.Sprintf("⚠️⚠️⚠️ 你的输入不会发送到该输出会话，使用 /%d 切换到当前输出的会话。", listIndex)
		content = panel.AppendRenderedPageNote(content, warning)
	}
	return content, nil
}

func modeConfirmation(entry CatalogEntry, mode hprp.OutputMode) string {
	name := "文本模式"
	if mode == hprp.OutputModeImage {
		name = "图片模式"
	}
	return fmt.Sprintf("已将 %s 设置为%s。", catalogTargetLabel(entry), name)
}

func statusNotificationText(entry CatalogEntry, status string) string {
	return catalogNotificationHeader(entry) + "\n" + statusNotificationTitle(status)
}

func catalogNotificationHeader(entry CatalogEntry) string {
	name := entry.Session.Display.DisplayAgent
	if name == "" {
		name = entry.Session.Display.Agent
	}
	if name == "" {
		name = entry.Session.SlotID
	}
	header := fmt.Sprintf("[%s/%d] %s", safeRouterLabel(entry.Ref.MachineID), entry.Session.Display.Index, safeRouterLabel(name))
	if entry.Session.Display.Title != "" {
		header += " — " + safeRouterLabel(entry.Session.Display.Title)
	}
	return header
}

func statusNotificationTitle(status string) string {
	switch hprp.NormalizeStatus(status) {
	case hprp.StatusWorking:
		return "Agent 开始工作。"
	case hprp.StatusBlocked:
		return "Agent 已阻塞，需要你的处理。"
	case hprp.StatusDone:
		return "Agent 已完成。"
	case hprp.StatusIdle:
		return "Agent 已空闲。"
	default:
		return "Agent 状态无法可靠识别，请在 Herdr 中确认。"
	}
}

func statusNeedsTerminal(previous, current string) bool {
	previous = hprp.NormalizeStatus(previous)
	current = hprp.NormalizeStatus(current)
	if current == hprp.StatusBlocked || current == hprp.StatusDone {
		return true
	}
	return current == hprp.StatusIdle && (previous == hprp.StatusWorking || previous == hprp.StatusBlocked)
}

func invalidatedNotificationText(target hprp.Target) string {
	return fmt.Sprintf("[%s] %s\nAgent 目标已失效，请重新执行 /ls 并选择可用会话。", safeRouterLabel(target.MachineID), safeRouterLabel(target.SlotID))
}

func catalogTargetLabel(entry CatalogEntry) string {
	workspace := panel.WorkspaceLabel(entry.Session.Display.Workspace, entry.Session.Display.Tab)
	agent := entry.Session.Display.Agent
	if agent == "" {
		agent = entry.Session.Display.DisplayAgent
	}
	if agent == "" {
		agent = entry.Session.SlotID
	}
	return fmt.Sprintf("[%s/%d] %s-%s(%s)",
		safeRouterLabel(entry.Ref.MachineID), entry.Session.Display.Index,
		safeRouterLabel(workspace), safeRouterLabel(agent), safeRouterLabel(entry.Session.SlotID))
}

type serverActionKind uint8

const (
	serverActionForward serverActionKind = iota
	serverActionList
	serverActionSelect
	serverActionHelp
	serverActionMode
	serverActionDirected
	serverActionRegister
	serverActionListRegistrations
	serverActionApproveRegistrations
	serverActionRejectRegistrations
)

type serverAction struct {
	kind        serverActionKind
	index       int
	content     string
	mode        hprp.OutputMode
	machineID   string
	sources     []string
	indexes     []int
	switchAfter bool
}

func parseServerAction(content string) (serverAction, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return serverAction{}, errors.New("输入不能为空。")
	}
	fields := strings.Fields(trimmed)
	if action, matched, err := parseDirectedAction(trimmed, fields[0]); matched {
		return action, err
	}
	switch fields[0] {
	case "/ls":
		if len(fields) != 1 {
			return serverAction{}, errors.New("/ls 用法: /ls")
		}
		return serverAction{kind: serverActionList}, nil
	case "/help":
		if len(fields) != 1 {
			return serverAction{}, errors.New("/help 用法: /help")
		}
		return serverAction{kind: serverActionHelp}, nil
	case "/mode":
		if len(fields) != 2 {
			return serverAction{}, errors.New("/mode 用法: /mode img 或 /mode txt")
		}
		var mode hprp.OutputMode
		switch fields[1] {
		case string(hprp.OutputModeImage):
			mode = hprp.OutputModeImage
		case string(hprp.OutputModeText):
			mode = hprp.OutputModeText
		default:
			return serverAction{}, errors.New("/mode 用法: /mode img 或 /mode txt")
		}
		return serverAction{kind: serverActionMode, mode: mode}, nil
	case "/reg":
		if len(fields) != 3 {
			return serverAction{}, errors.New("/reg 用法: /reg <机器标识> <来源地址1,来源地址2>")
		}
		sources := strings.Split(fields[2], ",")
		for _, source := range sources {
			if source == "" {
				return serverAction{}, errors.New("/reg 用法: /reg <机器标识> <来源地址1,来源地址2>")
			}
		}
		return serverAction{kind: serverActionRegister, machineID: fields[1], sources: sources}, nil
	case "/ls-reg":
		if len(fields) != 1 {
			return serverAction{}, errors.New("/ls-reg 用法: /ls-reg")
		}
		return serverAction{kind: serverActionListRegistrations}, nil
	case "/apr":
		indexes, err := parseRegistrationApprovalIndexes("apr", fields)
		if err != nil {
			return serverAction{}, err
		}
		return serverAction{kind: serverActionApproveRegistrations, indexes: indexes}, nil
	case "/rej":
		indexes, err := parseRegistrationApprovalIndexes("rej", fields)
		if err != nil {
			return serverAction{}, err
		}
		return serverAction{kind: serverActionRejectRegistrations, indexes: indexes}, nil
	}
	if len(fields) == 1 && strings.HasPrefix(fields[0], "/") {
		if index, err := positiveASCIIInt(strings.TrimPrefix(fields[0], "/")); err == nil {
			return serverAction{kind: serverActionSelect, index: index}, nil
		}
	}
	return serverAction{kind: serverActionForward}, nil
}

func parseDirectedAction(trimmed, prefix string) (serverAction, bool, error) {
	if len(prefix) < 2 || (prefix[0] != '/' && prefix[0] != '#') {
		return serverAction{}, false, nil
	}
	index, err := positiveASCIIInt(prefix[1:])
	if err != nil {
		return serverAction{}, false, nil
	}
	remainder := strings.TrimSpace(trimmed[len(prefix):])
	if remainder == "" {
		if prefix[0] == '/' {
			return serverAction{}, false, nil
		}
		return serverAction{}, true, errors.New("#N 用法: #N 内容")
	}
	nested, err := parseServerAction(remainder)
	if err != nil || (nested.kind != serverActionForward && nested.kind != serverActionMode) {
		return serverAction{}, true, errors.New("定向输入不能执行 /ls、/help、/reg、注册审批命令或另一个 /N。")
	}
	return serverAction{
		kind:        serverActionDirected,
		index:       index,
		content:     remainder,
		mode:        nested.mode,
		switchAfter: prefix[0] == '/',
	}, true, nil
}

func isRegistrationCommand(content string) bool {
	fields := strings.Fields(strings.TrimSpace(content))
	if len(fields) == 0 {
		return false
	}
	if fields[0] == "/reg" {
		return true
	}
	if len(fields) < 2 || fields[1] != "/reg" || len(fields[0]) < 2 || fields[0][0] != '/' && fields[0][0] != '#' {
		return false
	}
	_, err := positiveASCIIInt(fields[0][1:])
	return err == nil
}

func isRegistrationApprovalCommand(content string) bool {
	return registrationApprovalCommand(content) != ""
}

func registrationApprovalCommand(content string) string {
	fields := strings.Fields(strings.TrimSpace(content))
	if len(fields) == 0 {
		return ""
	}
	if fields[0] == "/ls-reg" || fields[0] == "/apr" || fields[0] == "/rej" {
		return fields[0]
	}
	if len(fields) < 2 || len(fields[0]) < 2 || fields[0][0] != '/' && fields[0][0] != '#' {
		return ""
	}
	if fields[1] != "/ls-reg" && fields[1] != "/apr" && fields[1] != "/rej" {
		return ""
	}
	_, err := positiveASCIIInt(fields[0][1:])
	if err != nil {
		return ""
	}
	return fields[1]
}

func isRegistrationApprovalAction(kind serverActionKind) bool {
	return kind == serverActionListRegistrations || kind == serverActionApproveRegistrations || kind == serverActionRejectRegistrations
}

func parseRegistrationApprovalIndexes(command string, fields []string) ([]int, error) {
	usage := fmt.Sprintf("/%s 用法: /%s 1 2 3", command, command)
	if len(fields) < 2 {
		return nil, errors.New(usage)
	}
	if len(fields)-1 > maxRegistrationApprovalBatch {
		return nil, fmt.Errorf("每次最多处理 %d 个注册申请，请重新执行 /ls-reg。", maxRegistrationApprovalBatch)
	}
	indexes := make([]int, 0, len(fields)-1)
	seen := make(map[int]struct{}, len(fields)-1)
	for _, field := range fields[1:] {
		index, err := positiveASCIIInt(field)
		if err != nil {
			return nil, errors.New(usage)
		}
		if _, exists := seen[index]; exists {
			return nil, errors.New("编号不能重复，请重新执行 /ls-reg。")
		}
		seen[index] = struct{}{}
		indexes = append(indexes, index)
	}
	return indexes, nil
}

func registrationErrorType(err error) string {
	switch {
	case errors.Is(err, machinereg.ErrMachineExists):
		return "machine_exists"
	case errors.Is(err, machinereg.ErrDeliveryFailed):
		return "delivery_failed"
	case errors.Is(err, machinereg.ErrRollbackFailed):
		return "rollback_failed"
	case errors.Is(err, machinereg.ErrInvalidRequest):
		return "invalid_request"
	default:
		return "internal"
	}
}

type unavailableRegistrationRequester struct{}

func (unavailableRegistrationRequester) Register(context.Context, machinereg.RegisterInput, machinereg.KeyDeliveryFunc) (machinereg.RegisterResult, error) {
	return machinereg.RegisterResult{}, machinereg.ErrInvalidRequest
}

type unavailableRegistrationApprovalHandler struct{}

func (unavailableRegistrationApprovalHandler) IsAdmin(string) bool { return false }

func (unavailableRegistrationApprovalHandler) NotifyPending(context.Context, machinereg.Request) error {
	return nil
}

func (unavailableRegistrationApprovalHandler) PrepareList(string) (RegistrationApprovalList, error) {
	return RegistrationApprovalList{}, ErrRegistrationApprovalUnauthorized
}

func (unavailableRegistrationApprovalHandler) CommitList(string, RegistrationApprovalList) error {
	return ErrRegistrationApprovalUnauthorized
}

func (unavailableRegistrationApprovalHandler) Approve(context.Context, string, []int) (string, error) {
	return "", ErrRegistrationApprovalUnauthorized
}

func (unavailableRegistrationApprovalHandler) Reject(context.Context, string, []int) (string, error) {
	return "", ErrRegistrationApprovalUnauthorized
}

func (unavailableRegistrationApprovalHandler) Invalidate(string) error {
	return ErrRegistrationApprovalUnauthorized
}

func registrationApprovalErrorType(err error) string {
	switch {
	case errors.Is(err, ErrRegistrationApprovalUnauthorized):
		return "unauthorized"
	case errors.Is(err, ErrRegistrationApprovalSnapshotMissing):
		return "snapshot_missing"
	case errors.Is(err, ErrRegistrationApprovalSnapshotChanged):
		return "snapshot_changed"
	case errors.Is(err, ErrRegistrationApprovalInvalidIndexes):
		return "invalid_indexes"
	default:
		return "internal"
	}
}

func registrationApprovalErrorMessage(kind serverActionKind, err error) string {
	message := "注册审批操作失败，请稍后重试或联系管理员。"
	switch {
	case errors.Is(err, ErrRegistrationApprovalUnauthorized):
		return ErrRegistrationApprovalUnauthorized.Error()
	case errors.Is(err, ErrRegistrationApprovalSnapshotMissing):
		message = ErrRegistrationApprovalSnapshotMissing.Error()
	case errors.Is(err, ErrRegistrationApprovalSnapshotChanged):
		message = ErrRegistrationApprovalSnapshotChanged.Error()
	case errors.Is(err, ErrRegistrationApprovalInvalidIndexes):
		message = "注册审批编号无效，请重新执行 /ls-reg。"
	}
	if kind == serverActionApproveRegistrations || kind == serverActionRejectRegistrations {
		return message + "\n\n" + registrationApprovalSnapshotReminder
	}
	return message
}

func positiveASCIIInt(value string) (int, error) {
	if value == "" {
		return 0, errors.New("empty")
	}
	for index := range value {
		if value[index] < '0' || value[index] > '9' {
			return 0, errors.New("not ascii number")
		}
	}
	number, err := strconv.Atoi(value)
	if err != nil || number <= 0 {
		return 0, errors.New("not positive")
	}
	return number, nil
}

func (router *ConversationRouter) reply(ctx context.Context, message im.IncomingText, content string) error {
	parts := panel.SplitMarkdown(content, panel.WeComContentLimit)
	if len(parts) == 0 {
		parts = []string{"回复内容无效。"}
	}
	if err := router.gateway.RespondMarkdown(ctx, message.RequestID, parts[0]); err != nil {
		args := []any{"user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "part_index", 1, "part_count", len(parts), "content_bytes", len([]byte(parts[0]))}
		router.logger.Warn("企业微信首段回复失败", append(args, serverErrorLogArgs(err)...)...)
		return err
	}
	router.logger.Debug("企业微信回复分段发送成功", "user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "part_index", 1, "part_count", len(parts), "content_bytes", len([]byte(parts[0])))
	for index, part := range parts[1:] {
		if err := router.gateway.SendMarkdownTo(ctx, message.UserID, part); err != nil {
			args := []any{"user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "part_index", index + 2, "part_count", len(parts), "content_bytes", len([]byte(part))}
			router.logger.Warn("企业微信后续回复失败", append(args, serverErrorLogArgs(err)...)...)
			return err
		}
		router.logger.Debug("企业微信回复分段发送成功", "user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "part_index", index+2, "part_count", len(parts), "content_bytes", len([]byte(part)))
	}
	router.logger.Debug("企业微信回复发送成功", "user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "part_count", len(parts), "content_bytes", len([]byte(content)))
	return nil
}

func (router *ConversationRouter) logInteractionError(message im.IncomingText, action, stage string, target hprp.Target, err error) {
	args := []any{"user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "action", action, "stage", stage}
	if target.MachineID != "" || target.SlotID != "" || target.SessionID != "" {
		args = append(args, targetLogArgs(target)...)
	}
	router.logger.Warn("企业微信交互路由失败", append(args, serverErrorLogArgs(err)...)...)
}

func (router *ConversationRouter) logInteractionSuccess(message im.IncomingText, action string, target hprp.Target) {
	args := []any{"user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "action", action}
	args = append(args, targetLogArgs(target)...)
	router.logger.Debug("企业微信交互路由成功", args...)
}

func safeRouterError(err error) string {
	switch {
	case errors.Is(err, ErrNoListSnapshot):
		return "尚无会话列表，请先执行 /ls。"
	case errors.Is(err, ErrSelectionIndexOutOfRange):
		return "会话编号超出范围，请重新执行 /ls。"
	case errors.Is(err, ErrTargetChanged):
		return "目标已变化，请重新执行 /ls 和 /N。"
	case errors.Is(err, ErrNoSelection):
		return "尚未选择 Agent，请先执行 /ls 和 /N。"
	case errors.Is(err, ErrUnknownConnection):
		return "目标机器已离线，请重新执行 /ls。"
	case errors.Is(err, ErrTerminalImageUnsupported):
		return "目标 Pal 不支持图片模式，请升级客户端或使用 /mode txt。"
	case errors.Is(err, context.DeadlineExceeded):
		return "请求超时，请检查目标机器连接。"
	default:
		return "操作失败，请稍后重试。"
	}
}

func sameSessionRef(left, right hprp.Target) bool {
	return left.MachineID == right.MachineID && left.SlotID == right.SlotID && left.SessionID == right.SessionID
}

func safeRouterLabel(value string) string {
	return strings.NewReplacer("\r", " ", "\n", " ", "\x00", "", "```", "``\u200b`").Replace(strings.ToValidUTF8(value, "�"))
}

func routerHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:8])
}
