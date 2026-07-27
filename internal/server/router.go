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

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/panel"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/relayproto"
)

var ErrInvalidRouterDependency = errors.New("ConversationRouter 依赖无效")

const defaultRelayRequestTimeout = 20 * time.Second

const noAvailableSessionsMessage = "当前没有可用会话，使用/userid 获取用户id，并配置接入herdr-pal，使用/help获取内置命令帮助"

const defaultHelpRelayURL = "wss://管理员提供的地址:9443"

const helpRelayURLToken = "{{RELAY_URL}}"

const serverHelpTextTemplate = "### Herdr Pal 快速上手\n\n" +
	"已经完成配置时：`/ls` 查看会话 → `/1` 选择会话 → 直接发送任务。\n\n" +
	"【基本控制】\n" +
	"`/userid` 获取当前企业微信用户 ID\n" +
	"`/ls` 列出所有在线机器和 Agent\n" +
	"`/N` 或 `/sel N` 选择第 N 个会话\n" +
	"`/con` 查看当前会话最近 100 行\n" +
	"`/pageup`、`/pagedn` 上下翻页\n" +
	"`/slash clear` 向 Agent 发送 `/clear`\n" +
	"普通文字直接发送给当前 Agent\n" +
	"`/help` 显示本帮助\n\n" +
	"【按键操作】\n" +
	"`/key up`、`/key down` 发送方向键\n" +
	"`/key space`、`/key esc` 发送空格或 Esc\n" +
	"`/enter` 等同 `/key enter`\n" +
	"`/key down,sp,dn,A,7` 连续发送多个按键\n\n" +
	"按键可用逗号或空格分隔，最多 32 个；`dn` 表示 down，`sp` 表示 space，也支持单个英文字母和数字。" +
	"按键间隔 100ms，完成后自动返回终端内容。Enter 只能单独发送。\n\n" +
	"【1. 安装 Herdr】\n" +
	"下载及安装说明：\n" +
	"https://herdr.dev/docs/install/\n\n" +
	"Linux/macOS：\n" +
	"`curl -fsSL https://herdr.dev/install.sh | sh`\n\n" +
	"Windows AMD64 Beta：\n" +
	"`powershell -ExecutionPolicy Bypass -c \"irm https://herdr.dev/install.ps1 | iex\"`\n\n" +
	"运行 `herdr`，启动需要远程操作的 Agent。可执行以下命令检查服务：\n\n" +
	"`herdr status server --json`\n\n" +
	"输出中的 `protocol` 应为 `17`。\n\n" +
	"【2. 安装 herdr-pal】\n" +
	"下载最新版本：\n" +
	"https://github.com/wenxichang/herdr-pal/releases/latest\n\n" +
	"选择对应文件：\n" +
	"- Apple Silicon：`herdr-pal-darwin-arm64`\n" +
	"- Intel Mac：`herdr-pal-darwin-amd64`\n" +
	"- Linux x64：`herdr-pal-linux-amd64`\n" +
	"- Linux ARM64：`herdr-pal-linux-arm64`\n" +
	"- Windows x64：`herdr-pal-windows-amd64.exe`\n\n" +
	"Linux/macOS 下载后执行：\n" +
	"`chmod +x herdr-pal-*`\n\n" +
	"【3. 创建 config.json】\n" +
	"放置位置：\n\n" +
	"Linux/macOS：\n" +
	"`~/.config/herdr-pal/config.json`\n\n" +
	"Windows：\n" +
	"`%USERPROFILE%\\.config\\herdr-pal\\config.json`\n\n" +
	"配置示例：\n\n" +
	"{\n" +
	"  \"relay\": {\n" +
	"    \"url\": \"" + helpRelayURLToken + "\",\n" +
	"    \"userid\": \"在机器人中发送 /userid 获取\",\n" +
	"    \"machine_id\": \"当前运行herdr的机器标识\",\n" +
	"    \"skip_verify\": true\n" +
	"  },\n" +
	"  \"herdr\": {\n" +
	"    \"session\": \"\",\n" +
	"    \"socket_path\": \"\"\n" +
	"  },\n" +
	"  \"log\": {\n" +
	"    \"level\": \"info\"\n" +
	"  }\n" +
	"}\n\n" +
	"字段说明：\n" +
	"- `relay.url`：向 Herdr Pal 服务管理员获取\n" +
	"- `relay.userid`：在机器人单聊中发送 `/userid` 获取\n" +
	"- `relay.machine_id`：替换为自行设置的机器标识，同一用户的每台机器不能重复；留空时使用系统 hostname\n" +
	"- `relay.skip_verify`：通常保持 `true`，管理员另有要求时按其说明填写\n" +
	"- `herdr.session`：默认会话留空；使用命名会话时填写会话名\n" +
	"- `herdr.socket_path`：通常留空，由程序自动探测\n" +
	"- `log.level`：通常使用 `info`\n\n" +
	"【4. 启动】\n" +
	"Linux/macOS：\n" +
	"`./herdr-pal-对应平台文件`\n\n" +
	"Windows：\n" +
	"`.\\herdr-pal-windows-amd64.exe`\n\n" +
	"启动成功后回到企微，发送 `/ls`，再用 `/N` 选择会话。"

func buildServerHelpText(relayURL string) string {
	relayURL = strings.TrimSpace(relayURL)
	if relayURL == "" {
		relayURL = defaultHelpRelayURL
	}
	return strings.Replace(serverHelpTextTemplate, helpRelayURLToken, relayURL, 1)
}

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

// SendPush 复核后续分段的稳定来源，并发送给所属企业微信用户。
func (router *ConversationRouter) SendPush(ctx context.Context, userID string, push relayproto.ExecutePush) error {
	if router == nil || strings.TrimSpace(userID) == "" {
		return ErrInvalidRouterDependency
	}
	entry, err := router.catalog.ResolveTarget(userID, push.Target)
	if err != nil {
		return err
	}
	content, err := router.decorateTerminalContent(userID, entry, push.Content)
	if err != nil {
		return err
	}
	parts := panel.SplitMarkdown(content, panel.WeComContentLimit)
	if len(parts) == 0 {
		return errors.New("后续分段内容无效")
	}
	for _, part := range parts {
		if err := router.gateway.SendMarkdownTo(ctx, userID, part); err != nil {
			return err
		}
	}
	return nil
}

// SendNotification 复核最新目录并补充机器、本地序号和 panel 标题。
func (router *ConversationRouter) SendNotification(ctx context.Context, userID, machineID string, notification relayproto.Notification) error {
	if router == nil || notification.Target.MachineID != machineID {
		return ErrTargetChanged
	}
	entry, err := router.catalog.ResolveTarget(userID, notification.Target)
	if err != nil {
		return err
	}
	if panel.IsRenderedPage(notification.Content) {
		content, err := router.decorateTerminalContent(userID, entry, notification.Content)
		if err != nil {
			return err
		}
		parts := panel.SplitMarkdown(content, panel.WeComContentLimit)
		if len(parts) == 0 {
			return errors.New("通知内容无效")
		}
		for _, part := range parts {
			if err := router.gateway.SendMarkdownTo(ctx, userID, part); err != nil {
				return err
			}
		}
		return nil
	}
	name := entry.Session.DisplayAgent
	if name == "" {
		name = entry.Session.Agent
	}
	header := fmt.Sprintf("[%s/%d] %s", safeRouterLabel(machineID), entry.Ref.LocalIndex, safeRouterLabel(name))
	if entry.Session.Title != "" {
		header += " — " + safeRouterLabel(entry.Session.Title)
	}
	parts := panel.SplitMarkdown(header+"\n"+notification.Content, panel.WeComContentLimit)
	if len(parts) == 0 {
		return errors.New("通知内容无效")
	}
	for _, part := range parts {
		if err := router.gateway.SendMarkdownTo(ctx, userID, part); err != nil {
			return err
		}
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
	helpText       string
	requestTimeout time.Duration
}

// ConversationRouterConfig 是服务端会话路由器的展示配置。
type ConversationRouterConfig struct {
	// RelayURL 是在 /help 客户端配置示例中展示的 WSS 地址。
	RelayURL string
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
	return &ConversationRouter{
		catalog: catalog, executor: executor, gateway: gateway, relay: relay,
		deduper: deduper, logger: logger, helpText: buildServerHelpText(config.RelayURL), requestTimeout: defaultRelayRequestTimeout,
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
		if !router.catalog.HasSessions(message.UserID) {
			router.reply(ctx, message, noAvailableSessionsMessage)
			return
		}
		router.reply(ctx, message, err.Error())
		return
	}
	if action.kind != serverActionUserID && action.kind != serverActionHelp && !router.catalog.HasSessions(message.UserID) {
		router.reply(ctx, message, noAvailableSessionsMessage)
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
		router.reply(ctx, message, router.helpText)
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
		fmt.Fprintf(&content, "\n   工作区：%s\n   状态：%s", safeRouterLabel(panel.WorkspaceLabel(entry.Session.Workspace, entry.Session.Tab)), safeRouterLabel(panel.AgentStatusLabel(herdr.AgentStatus(entry.Session.Status))))
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
	consoleMessage := message
	consoleMessage.Content = "/con"
	requestContext, cancel = context.WithTimeout(ctx, router.requestTimeout)
	content, err := router.relay.Execute(requestContext, message.UserID, entry.Ref, consoleMessage)
	cancel()
	if err != nil {
		router.reply(ctx, message, "已选择 "+catalogTargetLabel(entry)+"，但"+safeRouterError(err))
		return
	}
	if strings.TrimSpace(content) == "" {
		content = "已选择 " + catalogTargetLabel(entry) + "。"
	}
	decorated, decorateErr := router.decorateTerminalContent(message.UserID, entry, content)
	if decorateErr != nil {
		router.reply(ctx, message, "已选择 "+catalogTargetLabel(entry)+"，但"+safeRouterError(decorateErr))
		return
	}
	router.reply(ctx, message, decorated)
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
	decorated, decorateErr := router.decorateTerminalContent(message.UserID, entry, content)
	if decorateErr != nil {
		router.reply(ctx, message, safeRouterError(decorateErr))
		return
	}
	router.reply(ctx, message, decorated)
}

func (router *ConversationRouter) decorateTerminalContent(userID string, source CatalogEntry, content string) (string, error) {
	if !panel.IsRenderedPage(content) {
		return content, nil
	}
	listIndex, err := router.catalog.EnsureNumberedIndex(userID, source.Ref)
	if err != nil {
		return "", err
	}
	content = panel.DecorateRenderedPage(content, source.Ref.MachineID, source.Ref.LocalIndex, listIndex)
	selected, selectedErr := router.catalog.Selected(userID)
	if selectedErr == nil && !sameSessionRef(selected.Ref, source.Ref) {
		warning := fmt.Sprintf("⚠️⚠️⚠️[当前会话] %s, 你的输入将不会发送给当前输出的会话，使用 /%d 切换到当前输出的会话。", catalogTargetLabel(selected), listIndex)
		content = panel.AppendRenderedPageNote(content, warning)
	}
	return content, nil
}

func catalogTargetLabel(entry CatalogEntry) string {
	workspace := panel.WorkspaceLabel(entry.Session.Workspace, entry.Session.Tab)
	agent := entry.Session.Agent
	if agent == "" {
		agent = entry.Session.DisplayAgent
	}
	if agent == "" {
		agent = entry.Session.PaneID
	}
	return fmt.Sprintf("[%s/%d] %s-%s(%s)",
		safeRouterLabel(entry.Ref.MachineID), entry.Ref.LocalIndex,
		safeRouterLabel(workspace), safeRouterLabel(agent), safeRouterLabel(entry.Session.PaneID))
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
