# Herdr Pal 交接上下文

## 1. 当前状态

Herdr Pal 已从单用户、客户端直连企业微信的原型演进为多用户、多机器 Relay 架构：

- `herdr-pal-server` 独占一个企业微信智能机器人长连接，并监听 WSS Relay。
- 每台运行 Herdr 的机器启动一个 `herdr-pal`，连接本机 Herdr 公共 Socket，并向服务端
  上报 `(userid, machine_id)` 及全部 Agent 会话。
- 当前 Relay 协议版本为 3；`execute_push` 从版本 2 起携带稳定 `SessionRef`，版本 3 的
  `execute_response` 可以回传客户端已确认的新选择。
- 用户在企业微信单聊中执行 `/ls`，获得自己所有在线机器的聚合列表；选择后，服务端使用
  稳定的机器、pane 和 occupant 身份路由请求。
- `herdr-pal -i` 继续提供不经过网络的本机控制台模式。

`build.sh` 使用 `CGO_ENABLED=0` 同时生成 Darwin/Linux 的 AMD64、ARM64 客户端和服务端，
以及 Windows AMD64 客户端 Beta。文件名均包含操作系统及架构；另外保留
`dist/herdr-pal` 和 `dist/herdr-pal-server` 作为当前构建机器的便捷名称。Windows 暂不
发布服务端。所有运行状态保存在内存中。

网络模式未指定 `-config` 时，`herdr-pal-server` 默认读取
`~/.config/herdr-pal/server-config.json`，`herdr-pal` 默认读取
`~/.config/herdr-pal/config.json`。`herdr-pal -i` 继续允许无配置运行，不自动加载该默认
客户端配置。

当前实现包括：

- 企业微信智能机器人 API 模式 WebSocket 长连接、订阅、心跳、响应关联和重连。
- Relay 严格版本化 JSON 协议、WSS 连接中心、自签名证书持久化和有界请求队列。
- 多用户会话目录、按用户串行执行器、全局编号快照、稳定目标选择和跨机器通知。
- Relay 客户端自动重连、变化时即时快照、30 秒校准快照和本地 Bridge 执行。
- Herdr protocol 17 门禁、快照、生命周期订阅、pane 级状态订阅和重连恢复。
- 100 行分页、状态确认 prompt、一次性 Enter 恢复、受限按键、通知和消息幂等。
- 结构化安全日志、进程锁、SIGINT/SIGTERM 和有界优雅退出。
- fake Herdr、fake 企业微信、Relay 协议测试和多用户多机器端到端测试。

## 2. 固定产品决策

- IM 平台：企业微信智能机器人长连接，由服务端唯一持有 Bot ID 和 Secret。
- 用户范围：不在 Herdr Pal 中配置用户白名单；企业微信应用可见范围是入口边界，只处理
  单聊。
- 客户端身份：用户把机器人 `/userid` 返回的完整值原样写入客户端配置。
- 机器标识：客户端 `machine_id` 可以显式配置；留空时使用系统 hostname，并继续执行 Relay
  身份格式校验。
- 机器唯一性：`(userid, machine_id)` 是在线唯一键；后来建立的重复连接被拒绝。
- 多用户命名：不同用户可以使用相同 `machine_id`。
- 在线目录：客户端断线或心跳超时后立即移除机器和全部会话，不保留离线目录。
- 技术栈：Go 1.26.5 或更高兼容补丁版本、`CGO_ENABLED=0`、单文件分发、手工启动。
- Relay 安全：只支持 WSS；客户端 `skip_verify` 默认 `true`，适用范围限定为受信任内网。
- Herdr 集成：只使用公共 Local Socket API、公共 CLI 和审计过的 JSON 模型。
- 协议：只接受精确 protocol 17，不能按“17 或更高”处理。
- 持久化：选择、目录、通知 hash、分页、幂等键和断线消息均不持久化或重放。
- 输出：只称为终端近期快照，不承诺完整对话或结构化 LLM transcript。
- 审批：不自动批准权限请求；用户只能显式发送白名单按键。

## 3. 网络模式数据流

### 3.1 服务端启动

1. `config.LoadServer` 读取 Bot ID、监听地址、客户端可访问的 `addr_hint` 和 TLS 路径，从
   `HERDR_PAL_WECOM_SECRET` 读取 Secret。
2. `EnsureTLS` 加载外部证书；若未配置则在状态目录生成并复用自签名证书，私钥 `0600`。
3. 创建 `SessionCatalog`、`ClientHub`、`UserExecutor` 和 `ConversationRouter`。
4. 启动唯一企业微信连接和 TLS HTTP/WebSocket 监听。
5. 用户发送 `/userid` 时，直接返回企业微信回调中的 userid，不要求已有客户端在线。
6. 用户发送 `/help` 时，服务端把 `addr_hint` 与 `listen` 端口组合为客户端 WSS URL，并
   写入返回的 `config.json` 示例；未配置 `addr_hint` 时保留管理员地址占位提示。

### 3.2 客户端启动

1. `config.LoadClient` 严格校验 `wss://` 和 userid；machine_id 留空时先使用系统 hostname，
   再执行 Relay 身份格式校验。
2. 解析本机 Herdr Socket，获取对应 Socket 的进程锁。
3. 启动 `EventSupervisor`，按最新 `session.snapshot` 接管本机会话。
4. 建立 Relay WSS，发送 `client_hello` 和首个完整 `session_snapshot`。
5. EventSupervisor 每 10 秒主动重新读取一次 Herdr 权威 `session.snapshot`；Registry 变化后，
   Relay 客户端最多约 250ms 上报新快照，并按服务端协商间隔发送校准快照。
6. Relay 断线时不缓存 prompt、按键或通知，指数退避重连。

### 3.3 企业微信输入

- `/userid`、`/ls`、独立的 `/N`/`/sel N`、`/help` 由服务端处理。
- `/N 内容` 和 `#N 内容` 由服务端解析全局编号，再把后续内容定向转发；前者仅在执行成功
  后切换当前选择，后者保持当前选择。定向前缀不能嵌套，也不能包装其他服务端全局命令。
- 其余内容要求已有稳定选择，然后作为 `execute_request` 发给目标机器。
- 客户端先用稳定引用选择本地目标，再进入原有 `Bridge Service`。
- 服务端等待首段 `execute_response`；后续分段通过携带稳定目标的 `execute_push` 主动发送。
- prompt 成功后若同一 pane 的 Agent 会话发生切换，客户端先立即上报新快照，再在
  `execute_response` 中回传新 `SessionRef`；Router 根据普通执行、`/N` 或 `#N` 的语义
  决定重绑、切换或保持选择，Hub 不直接修改用户选择。
- 超时时服务端不自动重试，避免可能已经提交的 prompt 或按键重复执行。

### 3.4 状态通知

本地 `Notifier` 仍只读取最近 100 行，通过 Relay 发送结构化目标和内容。服务端复核该
目标仍存在于最新目录，再补充：

```text
[machine_id/local_index] DisplayAgent — panel title
```

然后向该连接所属 userid 主动推送。客户端断线时通知直接失败，不进入离线队列。

服务端按 userid 维护 2 分钟滑动活跃窗口。有效用户输入、当前会话终端事件、后台终端事件
和 `execute_push` 都会刷新时间；窗口内其他会话的渲染终端通知只发送机器、全局编号和
Workspace/Tab 摘要，提示使用 `/N` 切换。窗口外或没有当前选择时仍发送完整终端页，
`execute_push` 始终保持完整，避免丢失正在执行命令的后续内容。

## 4. 已实现命令

```text
/userid
/ls
/N
/sel N
/N TEXT
#N TEXT
/help
/con
/pageup
/pagedn
/enter       /key enter
/key KEYS
/slash TEXT
```

`/userid` 只在企业微信服务端模式有意义。独立的 `/<NUM>` 等同 `/sel <NUM>`；带后续
内容时，`/N` 表示成功后切换，`#N` 表示不切换。全局 `/ls` 条目
使用 `[machine_id/local_index]` 并包含 Agent、panel 标题、workspace、tab 和状态。
列表状态统一显示为 `done ✅`、`working ⏳`、`blocked ⁉️`、`idle 💤` 或 `unknown ❔`。
选择成功后立即在目标机器执行 `/con`，将最新 100 行作为选择回复。

`/key KEYS` 支持逗号或空白混合分隔，允许 `up`、`down`、`enter`、`esc`、`space`、
单个 ASCII 字母或数字，并支持 `dn -> down`、`sp -> space`。每条命令最多 32 个按键，
相邻按键间隔 100ms；`enter` 只能单独发送。按键结束后等待 200ms，再自动执行一次 `/con`。

普通文本发送前实时调用 `agent.get`，仅允许 `idle`、`done`。首次使用带 wait 的
`agent.prompt`；收到 `agent_prompt_stalled` 后，只有原 occupant、原状态序列和可输入状态
都保持不变时才补发一次 Enter，并继续等待状态变化。

分页固定 100 行一页。`/con` 读取最后 100 行并重置页码，`/pageup` 最多扩展到 1000
行，`/pagedn` 只访问缓存。`blocked`、`done` 和需要输出的 `idle` 通知也只读最近 100 行。
页码使用当前缓存范围，例如初次 `/con` 为 `[1/1]`，上翻扩展后为 `[2/2]`。
企业微信终端页在开头和末尾显示 `[终端输出#N]`，`N` 对应服务端当前全局 `/ls` 编号；
尚无编号快照时自动按在线目录建立。机器、`Workspace/Tab`、pane 和页码摘要放在末尾；来源与
当前选择不同时，非活跃窗口内额外附加 `⚠️⚠️⚠️[当前会话]` 告警、当前选择摘要，并提示
使用对应 `/N` 切换到当前输出会话；活跃窗口内改发 `⚠️ [machine/N] ... 有新的输出`
简短提醒。本机交互模式不添加全局编号标题。

## 5. Herdr API 关键事实

每个健康周期按以下顺序建立：

1. `ping`，要求 protocol 精确等于 17。
2. discovery `session.snapshot`。
3. 建立通用 pane lifecycle 订阅。
4. 为当前 Agent pane 建立批量 `pane.agent_status_changed` 订阅，每项包含 `pane_id`。
5. 再读取权威 `session.snapshot`，消除 snapshot 与订阅确认之间的窗口。
6. pane/occupant 集合变化时重建状态订阅，直到计划稳定。
7. 健康周期内每 10 秒主动读取权威 snapshot，弥补 lifecycle 事件缺失或延迟。
8. 用基线替换 Registry，不发送历史状态通知，然后开放输入和 Relay 上报。

Herdr 断线时 `Service.CurrentTargets()` 返回空目录，使 Relay 客户端立即上报会话撤下；
重连后完全以新快照为准。

不要依赖以下能力：

- `PaneReadResult.revision`：审计基线中固定为 `0`。
- `pane.output_changed`：只有 Schema 类型，没有可用公共输出流。
- 外部订阅 cursor 或 exactly-once 重放：当前不存在。

`agent.read(recent_unwrapped)` 返回终端快照，可能包含 prompt、回复、工具日志、spinner、
状态栏、权限界面和 TUI 重绘。

## 6. Relay 协议与目录语义

- 所有帧包含协议版本和严格类型，未知字段、超限帧和非法身份会被拒绝。
- 握手顺序为 `client_hello` → `server_hello` → 首个 `session_snapshot`。
- 服务端每 10 秒心跳，30 秒未收到 pong 关闭连接。
- 客户端变化检测默认 250ms，服务端要求 30 秒完整校准快照。
- 服务端目录只接受连接自己的 userid 和 machine_id，不允许一条连接上报其他机器。
- 全局编号只引用最近一次用户 `/ls` 快照；真正选择保存 `SessionRef`。
- pane 关闭、occupant 替换、新快照删除或连接断开都会使旧选择失效。
- 只有执行响应明确确认同一机器、同一 pane 的新 occupant 时，服务端才等待对应快照并按
  当前命令语义更新或保持选择；不会仅凭普通 occupant 变化猜测新目标。
- Relay 请求和出站队列均有容量上限，不使用无界 goroutine 或无界缓存。

## 7. 代码边界

- `cmd/herdr-pal`：Relay 客户端与 `-i` 的公开 CLI。
- `cmd/herdr-pal-server`：中央服务端 CLI。
- `internal/serverapp`：服务端配置、TLS、企微与 HTTP 生命周期装配。
- `internal/server`：在线目录、用户执行器、WSS Hub 和企业微信全局路由。
- `internal/relayproto`：严格、版本化的 Relay 帧和校验。
- `internal/relayclient`：WSS 重连、会话快照和本地执行请求。
- `internal/herdr`：公共 NDJSON 请求、严格响应模型、订阅、Unix Socket/Windows Named
  Pipe 平台传输和 endpoint 解析。
- `internal/session`：本机会话索引、编号、选择和 occupant 身份。
- `internal/command`：客户端本地命令解析。
- `internal/panel`：终端规范化、100 行分页和 UTF-8 安全分段。
- `internal/policy`：按键白名单、消息幂等和审计模型。
- `internal/bridge`：本地 Service、Notifier 和 EventSupervisor。
- `internal/interactive`：stdin/stdout 本地聊天适配器。
- `internal/im`：与具体 IM SDK 无关的入站、回复和通知模型。
- `internal/wecom`：企业微信协议、请求关联、心跳和重连。
- `internal/testkit`：仅测试使用的 protocol 17 fake。

旧的直连企微装配仍有内部兼容代码和测试，但公开 CLI 已不再暴露 `-discover-user`，网络
模式只走 Relay。不要在新功能中继续扩展旧直连路径。

## 8. 验证状态

标准验证：

```sh
./build.sh
./unittest.sh
go test -race ./...
```

Relay 端到端覆盖三个客户端：同一用户两台机器、另一用户使用相同 machine_id、列表聚合、
稳定选择、prompt 隔离和带机器/panel 标题的通知：

```sh
go test -race ./internal/integration \
  -run TestRelayEndToEndRoutesMultipleUsersAndMachines -count=3
```

可选真实 Herdr 只读测试：

```sh
PATH=/Users/wxc/Code/herdr/target/debug:$PATH \
HERDR_PAL_INTEGRATION=1 \
go test ./internal/integration -run '^TestRealHerdr$' -count=1 -v
```

实时 prompt 测试必须额外设置 `HERDR_PAL_LIVE_INPUT=1` 和明确的
`HERDR_PAL_LIVE_PANE_ID`，禁止对未知 pane 进行写入测试。

## 9. 安全边界与后续工作

- Herdr Socket 永远只在本机访问，不通过 Relay 原样代理。
- Bot Secret 只存在于 `herdr-pal-server` 环境变量，不进入客户端配置。
- Relay 当前没有客户端证书、共享 token 或服务端 userid 鉴权；`skip_verify` 默认开启。
  这是一项明确的受信任内网假设，不适合互联网部署。
- 日志不记录 Secret、Cookie、完整 prompt 或终端快照；userid 使用 hash，机器标识可见。
- 不允许 IM 调用 `server.stop`、`pane.close`、任意 `pane.send_input` 或自动审批。

后续优先评估：Relay 客户端认证和证书 pinning、跨重启 StateStore、可靠通知队列、服务
管理、配置迁移，以及第二个 IM Adapter。实时结构化输出仍应先形成 Herdr 公共 API 提案，
不能依赖私有 TUI 状态。
