package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/panel"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/relayproto"
)

var ErrInvalidRouterDependency = errors.New("ConversationRouter 依赖无效")

const defaultRelayRequestTimeout = 20 * time.Second

const serverHelpText = `输入帮助：
/userid             显示当前企业微信 userid
/ls                 列出全部在线机器上的 Agent
/N 或 /sel N        选择第 N 个 Agent
/help               显示本帮助
/con                 显示当前 Agent 最新 100 行并重置分页
/pageup、/pagedn    上翻、下翻缓存
/key KEYS           发送受限按键
/enter              等同 /key enter
/slash TEXT         将 /TEXT 作为普通消息发送给 Agent

除 /userid、/ls、选择和 /help 外，输入会转发给当前选择所在机器。`

// WeComGateway 是 Router 回复企业微信消息所需的动态用户发送能力。
type WeComGateway interface {
	RespondMarkdown(ctx context.Context, requestID, content string) error
	SendMarkdownTo(ctx context.Context, userID, content string) error
}

// RelayRequester 是 Router 复核选择和执行用户输入所需的客户端能力。
type RelayRequester interface {
	Select(ctx context.Context, userID string, target relayproto.SessionRef) error
	Execute(ctx context.Context, userID string, target relayproto.SessionRef, message im.IncomingText) (string, error)
}

// ConversationRouter 处理企业微信全局命令并把其他输入转发给选中机器。
type ConversationRouter struct {
	catalog        *SessionCatalog
	executor       *UserExecutor
	gateway        WeComGateway
	relay          RelayRequester
	deduper        *policy.Deduper
	logger         *slog.Logger
	requestTimeout time.Duration
}

// NewConversationRouter 创建多用户会话路由器。
func NewConversationRouter(catalog *SessionCatalog, executor *UserExecutor, gateway WeComGateway, relay RelayRequester, deduper *policy.Deduper, logger *slog.Logger) (*ConversationRouter, error) {
	if catalog == nil || executor == nil || gateway == nil || relay == nil || deduper == nil || logger == nil {
		return nil, ErrInvalidRouterDependency
	}
	return &ConversationRouter{
		catalog: catalog, executor: executor, gateway: gateway, relay: relay,
		deduper: deduper, logger: logger, requestTimeout: defaultRelayRequestTimeout,
	}, nil
}

// Handle 校验并按 userid 串行处理一条企业微信单聊文本。
func (router *ConversationRouter) Handle(ctx context.Context, message im.IncomingText) {
	if router == nil {
		return
	}
	if message.ChatType != "single" || strings.TrimSpace(message.UserID) == "" {
		router.logger.Warn("企业微信消息被服务端拒绝", "chat_type", message.ChatType, "user_hash", routerHash(message.UserID))
		return
	}
	if strings.TrimSpace(message.MessageID) == "" {
		router.reply(ctx, message, "消息标识缺失，未执行任何操作。")
		return
	}
	if !router.deduper.AddIfNew(message.MessageID) {
		router.logger.Info("企业微信重复消息已忽略", "user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID))
		return
	}
	err := router.executor.Submit(ctx, message.UserID, func(taskContext context.Context) error {
		router.handleAuthorized(taskContext, message)
		return nil
	})
	if errors.Is(err, ErrUserQueueFull) {
		router.reply(ctx, message, "当前用户输入队列已满，请稍后重试。")
	} else if err != nil && ctx.Err() == nil {
		router.logger.Warn("用户消息执行失败", "user_hash", routerHash(message.UserID), "error_type", "executor")
	}
}

func (router *ConversationRouter) handleAuthorized(ctx context.Context, message im.IncomingText) {
	action, err := parseServerAction(message.Content)
	if err != nil {
		router.reply(ctx, message, err.Error())
		return
	}
	switch action.kind {
	case serverActionUserID:
		router.reply(ctx, message, message.UserID)
	case serverActionList:
		router.handleList(ctx, message)
	case serverActionSelect:
		router.handleSelect(ctx, message, action.index)
	case serverActionHelp:
		router.reply(ctx, message, serverHelpText)
	default:
		router.handleExecute(ctx, message)
	}
}

func (router *ConversationRouter) handleList(ctx context.Context, message im.IncomingText) {
	entries := router.catalog.CreateNumberedSnapshot(message.UserID)
	if len(entries) == 0 {
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
		name := entry.Session.DisplayAgent
		if name == "" {
			name = entry.Session.Agent
		}
		fmt.Fprintf(&content, "\n%d. [%s/%d] %s", index+1, safeRouterLabel(entry.Ref.MachineID), entry.Ref.LocalIndex, safeRouterLabel(name))
		if entry.Session.Title != "" {
			fmt.Fprintf(&content, " — %s", safeRouterLabel(entry.Session.Title))
		}
		content.WriteString(marker)
		fmt.Fprintf(&content, "\n   工作区：%s / %s\n   状态：%s", safeRouterLabel(entry.Session.Workspace), safeRouterLabel(entry.Session.Tab), safeRouterLabel(entry.Session.Status))
	}
	content.WriteString("\n使用 /N 或 /sel N 选择目标。")
	router.reply(ctx, message, content.String())
}

func (router *ConversationRouter) handleSelect(ctx context.Context, message im.IncomingText, index int) {
	entry, err := router.catalog.ResolveNumbered(message.UserID, index)
	if err != nil {
		router.reply(ctx, message, safeRouterError(err))
		return
	}
	requestContext, cancel := context.WithTimeout(ctx, router.requestTimeout)
	err = router.relay.Select(requestContext, message.UserID, entry.Ref)
	cancel()
	if err != nil {
		router.reply(ctx, message, safeRouterError(err))
		return
	}
	if err := router.catalog.SetSelection(message.UserID, entry.Ref); err != nil {
		router.reply(ctx, message, safeRouterError(err))
		return
	}
	name := entry.Session.DisplayAgent
	if name == "" {
		name = entry.Session.Agent
	}
	router.reply(ctx, message, fmt.Sprintf("已选择 [%s/%d] %s。", safeRouterLabel(entry.Ref.MachineID), entry.Ref.LocalIndex, safeRouterLabel(name)))
}

func (router *ConversationRouter) handleExecute(ctx context.Context, message im.IncomingText) {
	entry, err := router.catalog.Selected(message.UserID)
	if err != nil {
		router.reply(ctx, message, "尚未选择 Agent，请先执行 /ls 和 /N。")
		return
	}
	requestContext, cancel := context.WithTimeout(ctx, router.requestTimeout)
	content, err := router.relay.Execute(requestContext, message.UserID, entry.Ref, message)
	cancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			router.reply(ctx, message, "操作可能已经提交，请先检查目标会话；服务端不会自动重试。")
			return
		}
		router.reply(ctx, message, safeRouterError(err))
		return
	}
	if strings.TrimSpace(content) == "" {
		content = "客户端已处理。"
	}
	router.reply(ctx, message, content)
}

type serverActionKind uint8

const (
	serverActionForward serverActionKind = iota
	serverActionUserID
	serverActionList
	serverActionSelect
	serverActionHelp
)

type serverAction struct {
	kind  serverActionKind
	index int
}

func parseServerAction(content string) (serverAction, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return serverAction{}, errors.New("输入不能为空。")
	}
	fields := strings.Fields(trimmed)
	switch fields[0] {
	case "/userid":
		if len(fields) != 1 {
			return serverAction{}, errors.New("/userid 用法: /userid")
		}
		return serverAction{kind: serverActionUserID}, nil
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
	case "/sel":
		if len(fields) != 2 {
			return serverAction{}, errors.New("/sel 用法: /sel N")
		}
		index, err := positiveASCIIInt(fields[1])
		if err != nil {
			return serverAction{}, errors.New("/sel 用法: /sel N")
		}
		return serverAction{kind: serverActionSelect, index: index}, nil
	}
	if len(fields) == 1 && strings.HasPrefix(fields[0], "/") {
		if index, err := positiveASCIIInt(strings.TrimPrefix(fields[0], "/")); err == nil {
			return serverAction{kind: serverActionSelect, index: index}, nil
		}
	}
	return serverAction{kind: serverActionForward}, nil
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

func (router *ConversationRouter) reply(ctx context.Context, message im.IncomingText, content string) {
	parts := panel.SplitMarkdown(content, panel.WeComContentLimit)
	if len(parts) == 0 {
		parts = []string{"回复内容无效。"}
	}
	if err := router.gateway.RespondMarkdown(ctx, message.RequestID, parts[0]); err != nil {
		router.logger.Warn("企业微信首段回复失败", "user_hash", routerHash(message.UserID), "error_type", "gateway")
		return
	}
	for _, part := range parts[1:] {
		if err := router.gateway.SendMarkdownTo(ctx, message.UserID, part); err != nil {
			router.logger.Warn("企业微信后续回复失败", "user_hash", routerHash(message.UserID), "error_type", "gateway")
			return
		}
	}
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
	case errors.Is(err, context.DeadlineExceeded):
		return "请求超时，请检查目标机器连接。"
	default:
		return "操作失败，请稍后重试。"
	}
}

func sameSessionRef(left, right relayproto.SessionRef) bool {
	return left.MachineID == right.MachineID && left.PaneID == right.PaneID && left.OccupantHash == right.OccupantHash
}

func safeRouterLabel(value string) string {
	return strings.NewReplacer("\r", " ", "\n", " ", "\x00", "", "```", "``\u200b`").Replace(strings.ToValidUTF8(value, "�"))
}

func routerHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:8])
}
