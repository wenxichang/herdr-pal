# Herdr Pal 交接上下文

## 1. 当前状态

Herdr Pal 第一版已经按 Go 单进程架构实现，目标是把 Herdr 与自己的企业微信智能机器人
单聊连接起来，也可以用 `herdr-pal -i` 在本机控制台运行同一套 Bridge。程序手工启动，
使用 `CGO_ENABLED=0` 构建为 `dist/herdr-pal` 单文件，运行状态只保存在内存中。

当前实现包括：

- 企业微信智能机器人 API 模式 WebSocket 长连接、订阅、心跳、`req_id` 响应关联和重连。
- Herdr 公共 Unix Socket NDJSON 客户端、精确 protocol 17 门禁、快照和两类事件订阅。
- 全部 Agent pane 的动态索引、occupant 身份校验和重连后最新状态接管。
- 单聊命令、100 行分页、带状态确认和一次性 Enter 恢复的普通 prompt、受限按键与
  `msgid` 幂等。
- 状态通知异步队列、失败重试、合并、去重和 pane 失效通知。
- 本机进程锁、结构化安全日志、SIGINT/SIGTERM 和 10 秒优雅退出。
- 无 TUI 的 ConsoleAdapter：stdin 接收聊天输入，stdout 输出回复与通知，Ctrl+D 正常退出。
- fake Herdr、fake 企业微信和核心端到端场景。

## 2. 固定产品决策

- IM 平台：企业微信智能机器人长连接。
- 用户范围：一个配置指定的企业微信帐号，只支持单聊。
- 技术栈：Go 1.26，单文件分发。
- 运行方式：手工启动和停止，不安装常驻服务。
- Herdr 集成：只使用公共 Local Socket API、公共 CLI 和审计过的 JSON 模型。
- 协议：只接受精确 protocol 17，不能按“17 或更高”处理。
- 状态恢复：进程重启和 Herdr 重连后以最新 `session.snapshot` 接管，不恢复旧选择或分页。
- 持久化：当前没有 StateStore；选择、分页、通知 hash 和带 TTL 的 `msgid` 幂等键均只
  存内存，进程重启后不恢复。
- 输出：只称为终端近期快照，不承诺完整对话、结构化 assistant 消息或 LLM transcript。
- 审批：不自动批准权限请求；用户可以显式发送白名单按键。唯一自动按键是普通 prompt
  已从 `idle`/`done` 提交但 5 秒无状态变化时，在 occupant、状态和序列复核后补发的一次
  `enter`；`blocked` 永不触发该恢复。

## 3. 已实现命令

```text
/ls
/sel N
/con
/pageup
/pagedn
/keyup       /key up
/keydn       /key down
/enter       /key enter
/esc         /key esc
/space       /key space
/key X
```

`X` 只允许单个 ASCII 字母或数字。其他不以 `/` 开头的文本通过 `agent.prompt` 发送；
未知 `/` 命令不会降级为 prompt。按键命令不二次确认。

普通文本发送前实时调用 `agent.get`，仅允许 `idle`、`done`。首次发送使用带 wait 的
`agent.prompt`，并以 `state_change_seq` 确认状态确实变化；收到
`agent_prompt_stalled` 后只允许一次受审计的 Enter 恢复，再轮询最多 5 秒。未观察到
变化时回复“发送未生效”，不会宣称已经开始执行。

分页固定 100 行一页。`/con` 读取最后 100 行并重置页码，`/pageup` 逐步扩大到最多
1000 行，`/pagedn` 只访问缓存。`blocked`、`done` 和需要输出的 `idle` 主动通知每次也
只读取最近 100 行，不读取全部新增内容。

## 4. Herdr API 关键事实

### 4.1 启动与重连

每个健康周期按以下顺序建立：

1. `ping`，要求 protocol 精确等于 17。
2. discovery `session.snapshot`。
3. 建立通用 pane lifecycle 订阅。
4. 为当前 Agent pane 建立批量 `pane.agent_status_changed` 订阅，每项显式包含 `pane_id`。
5. 再读取权威 `session.snapshot`，消除 snapshot 与订阅确认之间的窗口。
6. 若 pane/occupant 集合变化，重建状态订阅并再次 snapshot，直到计划稳定。
7. 用基线替换 Registry，不发送历史状态通知，然后开放输入。

Herdr 断线时立即进入 degraded，暂停 prompt/按键、清空选择和分页；重连后必须重新
`/ls`、`/sel`。

### 4.2 订阅的序列语义

不要笼统写成“所有订阅都从 sequence 0 重放”：

- 专用 `pane.agent_status_changed` 订阅在创建时取得 `current_sequence`，从该位置开始
  接收新状态，不读取订阅前保留的状态事件。
- 通用 lifecycle 订阅才会从 EventHub 当前保留队列的 sequence 0 读取，因此可能看到
  保留的 `pane.created`、`pane.updated` 等事件。

外部协议没有可持久化 cursor。重连仍必须依靠权威 snapshot 和业务幂等，而不是假定
事件 exactly-once 或可断点续传。

### 4.3 Prompt 确认语义

- protocol 17 的完整 `AgentInfo` 必须包含 `state_change_seq`；缺失时按协议错误处理。
- 普通文本初始状态只接受 `idle`、`done`，并使用带 wait 的 `agent.prompt` 覆盖全部
  稳定状态。
- Herdr 固定的 prompt effect 窗口内无变化会返回 `agent_prompt_stalled`。
- 恢复前再次调用 `agent.get`。occupant 已替换、序列已变化或状态不再可输入时不发送
  Enter。
- 只有仍为原 occupant、原序列且状态仍为 `idle`/`done` 时补发一次 Enter；随后通过
  `agent.get` 比较原序列，最多等待 5 秒。

### 4.4 输出限制

`agent.read(recent_unwrapped)` 返回终端快照，内容可能包含 prompt、Agent 回复、工具日志、
spinner、状态栏、权限界面和 TUI 重绘。`PaneReadResult.revision` 在审计基线中固定为 0，
没有可用的 `pane.output_changed` 公共流，也不能恢复已经丢失的 alternate-screen 历史。

## 5. 企业微信关键事实

- 连接官方 `wss://openws.work.weixin.qq.com`。
- 使用 `aibot_subscribe`、`aibot_msg_callback`、`aibot_respond_msg`、
  `aibot_send_msg`、`ping` 和 `disconnected_event` 必要子集。
- 主动单聊发送的 `chatid` 是 `allowed_user_id`，`chat_type` 为 `1`。
- 用户必须先给机器人发过消息，企业微信才允许主动推送；失败只在当前进程重试，不做
  持久化补发。
- Secret 只从 `HERDR_PAL_WECOM_SECRET` 读取，配置文件不接受 Secret 字段。
- `internal/app.Options.WeComEndpoint` 只为兼容端点和本地集成测试注入；CLI 和 JSON
  配置不暴露 endpoint，空值始终使用官方地址。

## 6. 代码边界

- `internal/herdr`：公共 NDJSON 请求、严格响应模型、订阅和 Socket 解析。
- `internal/wecom`：WebSocket 协议、请求关联、心跳和重连。
- `internal/interactive`：把 stdin/stdout 适配为单用户本地聊天会话。
- `internal/session`：快照索引、列表编号、选择和 occupant 身份。
- `internal/command`：纯命令解析。
- `internal/panel`：终端规范化、100 行分页和 UTF-8 安全分段。
- `internal/policy`：单用户单聊权限、按键白名单、幂等和审计模型。
- `internal/bridge`：入站 Service、状态 Notifier 和 EventSupervisor。
- `internal/app`：配置、进程锁、依赖装配、三个运行循环和退出协调。
- `internal/testkit`：仅供测试使用的公开 protocol 17 fake，不包含 Herdr 私有实现。

不要把 IM SDK、Herdr 协议、命令路由和输出提取合并成 god object。

## 7. 验证状态

常规验证：

```sh
./unittest.sh
./build.sh
go test ./internal/integration -run TestBridgeEndToEnd
```

端到端覆盖：启动订阅、`/ls`/`/sel`/prompt、stalled 后单次 Enter 恢复、working 状态
拒绝文本、显式 Enter/Space、分页读取次数、blocked/done 100 行、`msgid` 去重、未授权
与群聊、occupant 替换、Herdr 断线重选、企业微信断线不重放。

可选真实 Herdr 测试：

```sh
PATH=/Users/wxc/Code/herdr/target/debug:$PATH \
HERDR_PAL_INTEGRATION=1 \
go test ./internal/integration -run '^TestRealHerdr$' -count=1 -v
```

该只读测试先运行 `herdr status server --json`，只有 protocol 17 才继续执行真实
`ping`、`session.snapshot`、`agent.get`、`agent.read` 和交互模式 `/ls`、`/sel`、`/con`；
不访问企业微信外网，也不需要真实 Secret。

实时 prompt 测试必须显式指定目标 pane，并额外打开写入门禁：

```sh
PATH=/Users/wxc/Code/herdr/target/debug:$PATH \
HERDR_PAL_INTEGRATION=1 \
HERDR_PAL_LIVE_INPUT=1 \
HERDR_PAL_LIVE_PANE_ID='<必须替换为当前 pane ID>' \
go test ./internal/integration -run '^TestRealHerdrLivePrompt$' -count=1 -v
```

必须先从最新公共 `session.snapshot` 取得目标 pane ID 并替换占位符，禁止原样执行；未
替换或目标不存在时，测试应在调用 `agent.prompt` 前失败。门禁通过后只发送一次固定
marker prompt，并通过 `agent.read(recent_unwrapped, 100)` 等待新增 marker，不发送按键。

截至 2026-07-24，真实联调基线为源码 debug Herdr 0.7.5/protocol 17，二进制位于
`/Users/wxc/Code/herdr/target/debug/herdr`，配置目录为 `~/.config/herdr-dev`。默认
`PATH` 中的 Homebrew `/opt/homebrew/bin/herdr` 仍为 0.7.1，使用 `~/.config/herdr`；
两者解析到不同 Socket，因此运行测试或 `herdr-pal -i` 时必须显式使用 debug PATH。
只读测试和显式门禁的实时 prompt 测试均已在该 protocol 17 服务上完成联调。

## 8. 安全边界

- 不在 `/Users/wxc/Code/herdr` 中实现或修改 Herdr Pal。
- 不使用 MCP、plugin startup hook、私有 TUI socket、`AppState` 或未公开 Rust 模块。
- 不暴露原始 Herdr Socket 到网络。
- 不记录完整 Secret、Cookie、prompt 或终端快照。
- 未授权用户、群聊、未知 pane、失效 terminal/occupant 和 degraded 状态都不能产生输入。
- 白名单按键和 stalled prompt 的恢复 Enter 在取得当前选择后同步记录结构化审计；
  成功、occupant 拒绝、Herdr 查询或发送失败分别记录 `sent`、`rejected`、`failed`。
  审计只含用户、pane、occupant SHA-256 摘要、规范化按键、时间和结果，且不受普通日志
  级别过滤。
- 没有 `server.stop`、`pane.close`、`pane.send_text`、`pane.send_input` 或自动审批入口。

## 9. 后续工作

首版范围完成后再独立评估：多用户/群聊、持久化恢复、模板卡片、更多安全按键、
`launchd`、多媒体输入，以及官方 Go SDK 出现后的替换策略。任何需要实时结构化输出的
需求都应先形成 Herdr 公共 API 提案，不能依赖私有内部状态。
