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

	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/panel"
	"github.com/wenxichang/herdr-pal/internal/policy"
)

var ErrInvalidRouterDependency = errors.New("ConversationRouter 依赖无效")

const defaultRelayRequestTimeout = 20 * time.Second

const backgroundNotificationActivityWindow = 2 * time.Minute

const noAvailableSessionsMessage = "当前没有可用会话，使用/userid 获取用户 ID，并联系管理员签发机器 Key 后配置 herdr-pal；使用/help获取内置命令帮助"

const defaultHelpRelayURL = "wss://管理员提供的地址:9443"

const helpRelayURLToken = "{{RELAY_URL}}"

const serverHelpTextTemplate = "### Herdr Pal 快速上手\n\n" +
	"已经完成配置时：`/ls` 查看会话 → `/1` 选择会话 → 直接发送任务。\n\n" +
	"【基本控制】\n" +
	"`/userid` 获取当前企业微信用户 ID\n" +
	"`/ls` 列出所有在线机器和 Agent\n" +
	"`/N` 或 `/sel N` 选择第 N 个会话\n" +
	"`/N 内容` 在第 N 个会话执行，成功后切换；`#N 内容` 执行但不切换\n" +
	"`/con` 查看当前会话最近 100 行\n" +
	"`/pageup`、`/pagedn` 上下翻页\n" +
	"`/slash clear` 向 Agent 发送 `/clear`\n" +
	"普通文字直接发送给当前 Agent\n" +
	"`/help` 显示本帮助\n\n" +
	"定向前缀不能用于 `/userid`、`/ls`、`/help`、`/N` 或 `/sel N`。\n\n" +
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
	"    \"key\": \"管理员签发的 hpk_ 机器 Key\",\n" +
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
	"- `relay.key`：先发送 `/userid`，把返回值交给管理员；每台机器使用独立 Key\n" +
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
	Select(ctx context.Context, userID string, target hprp.Target) error
	Execute(ctx context.Context, userID string, target hprp.Target, message im.IncomingText) (RelayExecution, error)
}

// RelayExecution 是客户端首段回复及执行期间发生的 Agent 会话替换信息。
type RelayExecution struct {
	Content           string
	StructuredContent *hprp.Content
	SelectedTarget    *hprp.Target
}

// SendCommandOutput 复核后续分段的稳定来源，并发送给所属企业微信用户。
func (router *ConversationRouter) SendCommandOutput(ctx context.Context, userID string, output hprp.CommandOutput) error {
	if router == nil || strings.TrimSpace(userID) == "" {
		return ErrInvalidRouterDependency
	}
	entry, err := router.catalog.ResolveTarget(userID, output.Target)
	if err != nil {
		return err
	}
	router.activity.Touch(userID, router.now())
	content, err := router.decorateTerminalContent(userID, entry, output.Content.Text)
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
func (router *ConversationRouter) SendNotification(ctx context.Context, userID, machineID string, notification hprp.NotificationEvent) error {
	if router == nil || notification.Target.MachineID != machineID {
		return ErrTargetChanged
	}
	entry, err := router.catalog.ResolveTarget(userID, notification.Target)
	if err != nil {
		return err
	}
	recentlyActive := router.activity.RecentlyActiveAndTouch(userID, router.now(), backgroundNotificationActivityWindow)
	if panel.IsRenderedPage(notification.Content.Text) {
		selected, selectedErr := router.catalog.Selected(userID)
		if selectedErr == nil && !sameSessionRef(selected.Ref, entry.Ref) && recentlyActive {
			listIndex, err := router.catalog.EnsureNumberedIndex(userID, entry.Ref)
			if err != nil {
				return err
			}
			content := fmt.Sprintf("⚠️ %s 有新的输出，等待你的回复，使用/%d切换", catalogTargetLabel(entry), listIndex)
			return router.gateway.SendMarkdownTo(ctx, userID, content)
		}
		content, err := router.decorateTerminalContent(userID, entry, notification.Content.Text)
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
	name := entry.Session.Display.DisplayAgent
	if name == "" {
		name = entry.Session.Display.Agent
	}
	header := fmt.Sprintf("[%s/%d] %s", safeRouterLabel(machineID), entry.Session.Display.Index, safeRouterLabel(name))
	if entry.Session.Display.Title != "" {
		header += " — " + safeRouterLabel(entry.Session.Display.Title)
	}
	parts := panel.SplitMarkdown(header+"\n"+notification.Content.Text, panel.WeComContentLimit)
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
	activity       *userActivityTracker
	now            func() time.Time
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
		activity: newUserActivityTracker(), now: time.Now,
	}, nil
}

// Handle 校验并按 userid 串行处理一条企业微信单聊文本。
func (router *ConversationRouter) Handle(ctx context.Context, message im.IncomingText) {
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
	if strings.TrimSpace(message.MessageID) == "" {
		router.logger.Warn("企业微信消息被服务端拒绝", "user_hash", routerHash(message.UserID), "error_type", "missing_message_id", "reason", "企业微信消息缺少幂等标识")
		router.reply(ctx, message, "消息标识缺失，未执行任何操作。")
		return
	}
	if !router.deduper.AddIfNew(message.MessageID) {
		router.logger.Info("企业微信重复消息已忽略", "user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "reason", "消息幂等标识已处理")
		return
	}
	err := router.executor.Submit(ctx, message.UserID, func(taskContext context.Context) error {
		router.handleAuthorized(taskContext, message)
		return nil
	})
	if errors.Is(err, ErrUserQueueFull) {
		router.logger.Warn("企业微信消息排队失败", append([]any{"user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID)}, serverErrorLogArgs(err)...)...)
		router.reply(ctx, message, "当前用户输入队列已满，请稍后重试。")
	} else if err != nil && ctx.Err() == nil {
		router.logger.Warn("用户消息执行失败", append([]any{"user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID)}, serverErrorLogArgs(err)...)...)
	}
}

func (router *ConversationRouter) handleAuthorized(ctx context.Context, message im.IncomingText) {
	action, err := parseServerAction(message.Content)
	if err != nil {
		router.logger.Warn("企业微信交互解析失败", "user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "content_bytes", len([]byte(message.Content)), "error_type", "invalid_action", "reason", safeServerErrorReason(err))
		if !router.catalog.HasSessions(message.UserID) {
			router.reply(ctx, message, noAvailableSessionsMessage)
			return
		}
		router.reply(ctx, message, err.Error())
		return
	}
	router.logger.Debug("企业微信交互已接收", "user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "action", serverActionName(action.kind), "target_index", action.index, "switch_after", action.switchAfter, "content_bytes", len([]byte(message.Content)))
	router.activity.Touch(message.UserID, router.now())
	if action.kind != serverActionUserID && action.kind != serverActionHelp && !router.catalog.HasSessions(message.UserID) {
		router.logger.Info("企业微信交互未路由", "user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "action", serverActionName(action.kind), "error_type", "no_sessions", "reason", "当前用户没有在线且可用的 Relay 会话")
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
	case serverActionDirected:
		router.handleDirectedExecute(ctx, message, action)
	default:
		router.handleExecute(ctx, message)
	}
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
	content.WriteString("\n使用 /N 或 /sel N 选择目标。")
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
	content := result.Content
	if strings.TrimSpace(content) == "" {
		content = "已选择 " + catalogTargetLabel(entry) + "。"
	}
	decorated, decorateErr := router.decorateTerminalContent(message.UserID, entry, content)
	if decorateErr != nil {
		router.logInteractionError(message, "select", "decorate_console", entry.Ref, decorateErr)
		router.reply(ctx, message, "已选择 "+catalogTargetLabel(entry)+"，但"+safeRouterError(decorateErr))
		return
	}
	router.logInteractionSuccess(message, "select", entry.Ref)
	router.reply(ctx, message, decorated)
}

func (router *ConversationRouter) handleExecute(ctx context.Context, message im.IncomingText) {
	entry, err := router.catalog.Selected(message.UserID)
	if err != nil {
		router.logInteractionError(message, "execute", "resolve_selection", hprp.Target{}, err)
		router.reply(ctx, message, "尚未选择 Agent，请先执行 /ls 和 /N。")
		return
	}
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
	content := result.Content
	if strings.TrimSpace(content) == "" {
		content = "客户端已处理。"
	}
	decorated, decorateErr := router.decorateTerminalContent(message.UserID, entry, content)
	if decorateErr != nil {
		router.logInteractionError(message, "execute", "decorate_response", entry.Ref, decorateErr)
		router.reply(ctx, message, safeRouterError(decorateErr))
		return
	}
	router.logInteractionSuccess(message, "execute", entry.Ref)
	router.reply(ctx, message, decorated)
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
	content := result.Content
	if strings.TrimSpace(content) == "" {
		content = "客户端已处理。"
	}
	decorated, err := router.decorateTerminalContent(message.UserID, finalEntry, content)
	if err != nil {
		router.logInteractionError(message, "directed_execute", "decorate_response", finalEntry.Ref, err)
		router.reply(ctx, message, safeRouterError(err))
		return
	}
	router.logInteractionSuccess(message, "directed_execute", finalEntry.Ref)
	router.reply(ctx, message, decorated)
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
		warning := fmt.Sprintf("⚠️⚠️⚠️[当前会话] %s, 你的输入将不会发送给当前输出的会话，使用 /%d 切换到当前输出的会话。", catalogTargetLabel(selected), listIndex)
		content = panel.AppendRenderedPageNote(content, warning)
	}
	return content, nil
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
	serverActionUserID
	serverActionList
	serverActionSelect
	serverActionHelp
	serverActionDirected
)

type serverAction struct {
	kind        serverActionKind
	index       int
	content     string
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
	if err != nil || nested.kind != serverActionForward {
		return serverAction{}, true, errors.New("定向输入不能执行 /userid、/ls、/help、/N 或 /sel N。")
	}
	return serverAction{
		kind:        serverActionDirected,
		index:       index,
		content:     remainder,
		switchAfter: prefix[0] == '/',
	}, true, nil
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
		args := []any{"user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "part_index", 1, "part_count", len(parts), "content_bytes", len([]byte(parts[0]))}
		router.logger.Warn("企业微信首段回复失败", append(args, serverErrorLogArgs(err)...)...)
		return
	}
	router.logger.Debug("企业微信回复分段发送成功", "user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "part_index", 1, "part_count", len(parts), "content_bytes", len([]byte(parts[0])))
	for index, part := range parts[1:] {
		if err := router.gateway.SendMarkdownTo(ctx, message.UserID, part); err != nil {
			args := []any{"user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "part_index", index + 2, "part_count", len(parts), "content_bytes", len([]byte(part))}
			router.logger.Warn("企业微信后续回复失败", append(args, serverErrorLogArgs(err)...)...)
			return
		}
		router.logger.Debug("企业微信回复分段发送成功", "user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "part_index", index+2, "part_count", len(parts), "content_bytes", len([]byte(part)))
	}
	router.logger.Debug("企业微信回复发送成功", "user_hash", routerHash(message.UserID), "message_hash", routerHash(message.MessageID), "part_count", len(parts), "content_bytes", len([]byte(content)))
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
