# Herdr Pal 交接上下文

## 1. 当前状态

Herdr Pal 是 Herdr 与企业微信之间的独立 bridge：

- `herdr-pal-server` 独占企业微信智能机器人长连接，并监听 HPRP/1 WSS。
- 每台运行 Herdr 的机器启动一个 `herdr-pal`，只访问本机 Herdr 公共 Socket；Pal Bundle
  会复用或下载兼容名单中的 Herdr，并使用 Startup 插件触发快速 Launcher，由 Pal
  Supervisor 自己守护业务 Worker。
- Server 通过每机独立 Bearer Key 认证 Pal；Key 在服务端绑定企业微信用户 ID 和机器标识。
- `hp-cli` 通过本机 HPAP/1 Unix Socket 动态管理正在运行的 Server，不直接修改凭据文件。
- Pal 上报本机完整 Agent 会话快照，Server 为同一用户聚合多台机器并负责企业微信路由。
- `herdr-pal -i` 保留不经过网络的本机控制台模式。

网络模式默认配置：

- Server：`~/.config/herdr-pal-server/server.json`
- 管理员认证：`~/.config/herdr-pal-server/auth.json`
- 首次引导：`~/.config/herdr-pal-server/bootstrap.txt`
- 企业微信帮助：`~/.config/herdr-pal-server/help.md`
- Pal：`~/.config/herdr-pal/config.json`
- 凭据存储：默认 `<state_dir>/credentials.json`
- Admin Socket：默认 `<state_dir>/admin.sock`

`build.sh` 使用 `CGO_ENABLED=0` 生成 Darwin/Linux AMD64、ARM64 客户端和服务端，以及
Darwin/Linux AMD64、ARM64 `hp-cli` 和 Windows AMD64 客户端 Beta。当前平台另生成
`dist/herdr-pal`、`dist/herdr-pal-server` 和 `dist/hp-cli`。`unittest.sh` 运行全部单元、
本地集成和竞态测试。

## 2. 固定产品决策

- 企业微信 Bot ID 和 Secret 只由 Server 持有；Secret 直接保存在权限为 `0600` 的
  `server.json` 的 `wecom.secret` 字段中。
- 企业微信应用可见范围是用户入口边界；Router 只处理单聊。
- 企业微信 principal ID 和机器标识只由管理员在签发机器 Key 时绑定，不写入 Pal 配置。
- 管理员使用 `hp-cli key issue` 为每台机器签发独立 `hpk_...` Key，并必须配置至少一条
  来源地址规则。
- Key 在服务端绑定 `(principal_id, machine_id)`；Pal 不能自行声明或覆盖身份。
- 同一用户、同一机器标识只允许一条在线连接；后来连接返回 HTTP 409。
- 不同用户可以使用相同机器标识。连接断开后立即移除该机器和全部会话。
- Pal 与 Server 使用公开 HPRP/1；不兼容的主版本通过 WebSocket 子协议隔离。
- 网络只允许 WSS。`skip_verify` 默认开启仅用于受信任内网的自签名证书部署。
- Herdr 只接受精确 protocol 17，只使用公共 Local Socket API、CLI 和公开 Schema。
- 不持久化选择、在线目录、分页、通知或在途命令，不提供离线任务和通知回放。
- 终端显示模式按用户和完整稳定目标保存在 Server 内存中；OpenCode 默认图片，其他 Agent
  默认文本，Server 重启后恢复默认。
- 终端内容只描述为近期快照，不承诺完整对话或结构化 LLM transcript。
- 不自动批准权限请求；按键必须由用户显式触发并通过本地策略。
- HPAP 只支持 Unix Domain Socket；Windows 只构建 Pal，不构建 Server 或 `hp-cli`。
- 当前凭据文件格式为 version 2，不兼容 HPAP 实施前的无来源规则旧记录。

## 3. 部署与身份流程

### 3.1 Server

1. `config.LoadServer` 读取 Bot ID、监听地址、TLS 和凭据文件路径，并派生固定运行文件路径。
2. `EnsureTLS` 加载外部证书；未配置时在状态目录生成并复用自签名证书。
3. `credential.LoadStore` 加载仅保存摘要的 HPRP 机器凭据。
4. 创建 `SessionCatalog`、`ClientHub`、`UserExecutor` 和 `ConversationRouter`。
5. 在 `<state_dir>/admin.sock` 启动 HPAP 管理面；失败时整个 Server 启动失败。
6. 启动企业微信连接与 TLS HTTP/WebSocket 监听。
7. `/help` 每次重新读取 `help.md`，管理员修改后无需重启即可生效。

管理员从企业微信管理信息或组织内账户系统取得 principal ID 后执行：

```sh
hp-cli key issue \
  --principal-id '企业微信用户 ID' \
  --machine-id 'office-pc' \
  --source '192.168.1.20'
```

明文 Key 只在签发时输出一次。服务端保存 `credential_id`、绑定身份和 Secret SHA-256
摘要，不保存可直接使用的明文 Key。来源支持单 IP、CIDR 和同地址族闭区间；认证只使用 TLS
连接的真实 peer 地址，不信任代理头。

`credential_id` 在运行期按“当前最大值 + 1”分配。删除中间记录会留下空洞；如果尾部记录
全部删除，Server 重启后可能复用这些尾部数值，因此它只用于本机运行管理和审计定位，不是
永久全局标识。

### 3.2 Pal

1. `config.LoadClient` 严格校验 `wss://` 和 `relay.key`，并从 Key 派生安全日志使用的
   `credential_id`。
2. Startup 插件调用 `herdr-pal start`；Launcher 按规范化 Socket endpoint 保证幂等并启动
   脱离插件调用栈的 Supervisor。直接运行旧入口时仍按同一 endpoint 获取单实例进程锁。
3. Supervisor 通过 Herdr 公共 API 确认就绪后启动业务 Worker；Worker 启动
   `EventSupervisor`，以 Herdr 权威 `session.snapshot` 接管本机会话。
4. 使用 `Authorization: Bearer ...` 和子协议 `herdr-pal-relay.v1` 建立 WSS。
5. 完成 `hello.client/hello.server`，从服务端确认 Key 所绑定的 `machine_id`。
6. 上报并等待确认首个完整 `session.snapshot`，之后连接才进入 READY。
7. Registry 变化后最多约 250ms 上报快照；同时按校准周期强制上报完整快照。
8. 断线时不缓存 prompt、按键或通知，指数退避并从 hello、完整快照重新开始。
9. Supervisor 定时探测 Herdr 公共 `ping`；短暂不可连接进入宽限期，持续 5 秒不可连接后
   停止 Worker 并退出。Worker 自身异常退出时在 Herdr 存活期间退避重启。

## 4. HPRP/1 数据流

### 4.1 会话目录

HPRP 稳定目标为：

```text
machine_id + slot_id + session_id
```

- `machine_id` 来自服务端凭据绑定。
- `slot_id` 对应 Herdr pane。
- `session_id` 对应 pane 当前 Agent occupant。
- `/ls` 数字只是用户展示快照，实际路由始终保存完整稳定目标。

连接关闭、快照删除 pane 或同一 pane 更换 Agent 后，旧目标立即失效。

### 4.2 企业微信输入

- Server 先校验单聊身份和消息 ID，再完成 `msgid` 幂等去重；唯一输入随后进入按用户滚动
  窗口限速，默认每秒 1 条、60 秒内 20 条，显式配置 0 可关闭对应窗口。
- `/ls`、独立 `/N` 和 `/help` 由 Server 处理。
- `/N 内容` 与 `#N 内容` 先由 Server 把全局编号解析为稳定目标；前者仅在成功后切换，
  后者不改变当前选择。
- 其余内容要求已有稳定选择，Server 发送 `command.execute`。
- Pal 再次校验 Key 绑定机器、pane 和 occupant，之后才进入本地 Bridge Service。
- Pal 返回唯一 `command.result`；协商 `command.output.v1` 后再发送有序后续输出。
- Server 为每次 `command.execute` 显式携带 `output_mode`；Pal 不保存模式。
- 命令导致同一 pane 创建新 Agent 会话时，Pal 先上报并确认新快照，再在结果中返回
  `replacement_target`。
- Server 超时不自动生成新命令重试；相同 IM `msgid` 作为 `idempotency_key`。

Pal 在 10 分钟有界窗口内保存轻量幂等索引。相同 Key 与等价命令不会重复产生本地副作用；
同一 Key 对应不同目标或内容时返回 `command.idempotency_conflict`。图片和大段输出使用
独立 64 MiB 重放预算，预算不足时可省略可选正文但不能驱逐窗口内 Key；索引容量满时在
执行前以 `server.busy` 拒绝新 Key。

### 4.3 输出和通知

- `command.output` 必须引用原 `command.execute`，携带稳定目标和从 1 开始的连续序号。
- `notification.event` 使用 `event_key + sequence + target` 去重和排序。
- Server 只接受连接绑定机器的目标；状态事件目标必须仍存在于最新快照，
  `target.invalidated` 允许引用刚从目录移除的旧目标，跨机器上报会断开连接。
- 本地 Notifier 只上报状态元数据，不读取或附带终端正文。
- `done`、`blocked` 和需要关注的 `idle` 由 Server 按通知策略发送
  `terminal.snapshot.get`，Pal 无副作用读取最近 100 行；`working` 和 `unknown` 不读取正文。
- 图片模式使用最新 `session.snapshot` 几何和 `visible` ANSI 直接生成 PNG8，再独立读取
  同一目标的 `recent_unwrapped` 纯文本用于审计和降级；两者不要求逐行一致，失败时不修改
  会话模式。
- 命令和无副作用快照合并限制为一个本地在途请求；快照读取 15 秒超时，Server 对精确匹配
  已超时请求的迟到结果只记录并忽略，不断开连接。
- 同一用户最近 2 分钟有过任一方向交互时，其他会话通知降级为简短提醒。
- 断线期间的输出和通知直接失败，不排队等待下次连接。

Server 可把用户输入和终端文本作为 OTLP Logs 审计事件发送到配置的完整 HTTP(S) endpoint，
包括 Collector 的 `/v1/logs` 或 Loki 的 `/otlp/v1/logs`，也可独立输出 stderr JSON Lines。
审计使用每个目标独立的有界异步队列并始终 fail-open；图片模式记录配套纯文本而不是 PNG。
Bot Secret、OTLP Header、`hpk_...` 和常见认证 Header 在审计入队前脱敏，但剩余正文仍是
敏感数据。

## 5. Herdr API 关键事实

每个健康周期：

1. `ping`，要求 protocol 精确等于 17。
2. discovery `session.snapshot`。
3. 建立 pane lifecycle 订阅。
4. 为当前 Agent pane 建立 `pane.agent_status_changed` 订阅。
5. 再读取权威 snapshot，消除订阅建立窗口。
6. pane/occupant 变化时重建订阅直到稳定。
7. 每 10 秒重新读取权威 snapshot，弥补生命周期事件缺失或延迟。
8. 用基线替换 Registry，开放输入和 HPRP 上报。

不要依赖：

- `PaneReadResult.revision`：当前固定为 `0`。
- `pane.output_changed`：只有 Schema 类型，没有可用公共输出流。
- 外部订阅 cursor 或 exactly-once 重放：当前不存在。

## 6. 代码边界

- `internal/hprp`：HPRP/1 信封、公开消息、校验、协商和错误码。
- `internal/credential`：机器 Key 生成、摘要存储和 HTTP Bearer 验证。
- `internal/adminproto`、`internal/adminclient`、`internal/adminserver`：HPAP/1、本地客户端、
  Unix Socket 安全边界、管理方法和审计。
- `internal/server`：在线目录、HPRP Hub、用户执行器和企业微信路由。
- `internal/relayclient`：Pal 的 HPRP 连接、快照、命令幂等和输出上报。
- `internal/herdr`：公共 NDJSON 请求、订阅和平台传输。
- `internal/session`：本机会话索引、选择和 occupant 身份。
- `internal/bridge`：Service、Notifier 和 EventSupervisor。
- `internal/terminalimage`：Pal 侧内嵌字体、ANSI 终端页和 PNG8 渲染限制。
- `internal/command`、`internal/panel`、`internal/policy`：命令、终端和安全策略。
- `internal/wecom`、`internal/im`：企业微信协议与平台无关消息模型。

旧 `internal/relayproto` 已删除。不要重新引入客户端自声明 userid/machine_id 或协议 3
兼容分支。

## 7. 验证

提交前必须执行：

```sh
./unittest.sh
./build.sh
```

高风险网络改动额外执行：

```sh
go test -race ./internal/hprp ./internal/credential ./internal/server \
  ./internal/relayclient ./internal/integration
```

覆盖重点包括严格 JSON、Bearer 认证、hello/首快照、稳定目标、快照乱序、命令关联和
幂等、输出/通知去重、终端图片能力协商、同页文本、无副作用快照、分页、企业微信图片
上传和文本降级、多用户多机器隔离、Herdr 重连恢复、本地输入策略，以及 HPAP Key CRUD、
来源复核、连接/会话查询、动态 debug 和优雅停止。

## 8. 安全与后续工作

- Herdr Socket 永远只在本机访问，不通过 HPRP 原样代理。
- 日志不记录完整 Key、Secret、Cookie、prompt 或终端快照；用户、消息和 session 使用摘要。
- 服务器发来的内容仍必须经过 Pal 的 `PolicyGuard`，不构成自动授权。
- 当前 Key 是逻辑机器绑定，不提供硬件证明；复制 Key 仍可能在其他设备使用。
- 当前适合受信任内网。Server 已提供基础按用户输入限速和可选 OTLP 审计；互联网部署仍应
  补充证书信任、边界防护、OTLP 服务访问控制和数据保留策略。Key 的动态禁用、删除和来源
  收紧已经由 HPAP 提供。
- 后续 Feature 必须按用户功能建模，由 Pal 展开为多个 Herdr 公共 API 操作，不提供通用
  Herdr RPC 透传。
