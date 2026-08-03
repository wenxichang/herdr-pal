# Herdr Pal 自动启动与自守护设计

## 1. 背景

当前一体化安装包通过 Herdr 的 `[[sidecar]]` 启动、重启和停止 `herdr-pal`。如果需要在
不要求 Herdr 提供 Sidecar 能力的情况下保持接近的使用体验，可以复用 Herdr 已有插件
`[[startup]]` 钩子作为启动触发器，并把生命周期管理放回 Herdr Pal。

`[[startup]]` 只负责通知“Herdr 公共 Socket 已经就绪”，不负责长期进程的退出和重启。
因此不能直接用一个长期运行的 Pal 进程占据 startup 命令，而需要由 Pal 提供短生命周期
Launcher、后台 Supervisor 和实际 Worker。

本文以当前 `v0.5.1` 代码为基线，设计一种不修改 Herdr 源码、不依赖系统服务、只使用
Herdr 公共 Socket API 和插件 CLI 的自动启动方案。

## 2. 目标与非目标

### 2.1 目标

- Herdr Server 公共 Socket 就绪后自动启动 Pal。
- Herdr 重复执行 startup 或发生 live-handoff 时不产生重复 Pal 实例。
- Pal Worker 异常退出而 Herdr 仍然存活时自动恢复。
- Herdr 公共 Socket 短暂切换时保持运行，不误判为退出。
- Herdr Server 真正退出后，Pal 在有限时间内自动结束。
- 保留当前 Pal 的 HPRP、会话上报、终端读取、Relay 重连和日志语义。
- 整套机制只安装在当前用户目录，不要求 `launchd`、`systemd` 或 root 权限。
- Sidecar 模式和新模式共享同一个 Pal 二进制，避免维护两套业务实现。

### 2.2 非目标

- 不修改 `/Users/wxc/Code/herdr` 或新增 Herdr 私有接口。
- 不通过 Herdr 私有 TUI Socket、内部状态或父进程 PID 判断生命周期。
- 不把 HPRP Server 或 Herdr Socket 暴露到新的网络端口。
- 第一阶段不支持 Windows 的插件自守护安装；Windows 继续保留手工启动能力。
- 不让 Launcher 或 Supervisor处理会话、IM 命令和终端内容。
- 不保证在 Supervisor 自身遭到 `SIGKILL`、内核终止或机器断电后由 Pal 自己复活；下一次
  Herdr startup 会重新拉起它。

## 3. 当前能力与缺口

| 能力 | 当前状态 | 本设计处理 |
| --- | --- | --- |
| HPRP 长连接重连 | `relayclient.Client` 已实现 | 直接复用 |
| Herdr 连接、快照和订阅重建 | `bridge.Supervisor` 已实现 | 直接复用 |
| 每 10 秒刷新会话 | 已实现 | 直接复用 |
| Socket 规范化和单实例锁 | 已实现 | 锁的所有者调整为进程 Supervisor |
| 优雅取消和组件退出等待 | 已实现 | Worker 内继续复用 |
| Herdr 就绪后自动启动 | 目前由 `[[sidecar]]` 提供 | 改由插件 `[[startup]]` 触发 Launcher |
| Pal 进程崩溃恢复 | 目前由 `[[sidecar]]` 提供 | 新增 Supervisor 启动和重启 Worker |
| Herdr 真正退出后结束 Pal | 目前由 `[[sidecar]]` 提供 | 新增公共 API 存活探测和退出宽限期 |
| startup 重入和 handoff 竞争 | 当前不需要 Pal 处理 | 新增启动串行锁和本地健康控制端点 |

## 4. 方案比较

### 4.1 startup 直接运行 Pal

插件直接执行长期运行的 `herdr-pal`。实现最少，但 Pal 崩溃后无法恢复，也不能可靠处理
重复 startup 和 handoff 竞争。该方式只适合开发调试，不作为生产方案。

### 4.2 startup + Launcher + Supervisor + Worker

startup 只执行快速退出的 Launcher；Supervisor 脱离插件进程后管理 Worker 和 Herdr
生命周期。这种方式不依赖系统服务，并能覆盖 Sidecar 的主要守护语义。

这是本设计采用的方案。

### 4.3 用户级系统服务

使用 `launchd` 或 `systemd --user` 可以直接获得进程守护，但 Pal 会在 Herdr 之前长期
运行，部署方式也随操作系统分裂，不符合当前一体化包“随 Herdr 安装和启动”的目标。

## 5. 进程模型

```text
Herdr Server
    │
    │ 插件 [[startup]]，注入 HERDR_SOCKET_PATH
    ▼
herdr-pal start                         短生命周期 Launcher
    │ 解析身份、串行化启动、等待 Supervisor 就绪
    └──────────────────────────────────────────────┐
                                                   ▼
herdr-pal supervise                    后台 Supervisor
    │ 持有实例锁、提供本地健康端点、探测 Herdr
    │
    └── herdr-pal worker               业务 Worker
            ├── Herdr 公共 Socket
            └── HPRP Relay 长连接
```

三个角色使用同一个二进制，通过子命令区分。Launcher 和 Supervisor 不读取或处理终端
内容；Worker 继续运行当前 `app.RunRelay` 业务链路。

### 5.1 Launcher

Launcher 对外提供：

```text
herdr-pal start [-config /path/to/config.json]
```

职责如下：

1. 根据显式配置、`HERDR_SOCKET_PATH`、Herdr CLI 和默认路径解析 Herdr Socket。
2. 规范化 Socket 身份并计算实例哈希。
3. 获取短生命周期的启动串行锁，避免多个 startup 同时创建 Supervisor。
4. 查询已经存在的 Supervisor 健康端点。
5. 没有健康实例时，以脱离当前终端的方式启动 Supervisor。
6. 等待 Supervisor 完成初始 Herdr 探测并启动 Worker，随后返回成功。

Launcher 最长等待一个有限的启动窗口，建议默认 5 秒。它不得把 Pal 日志继续连接到
Herdr 插件命令的 stdout 或 stderr。

### 5.2 Supervisor

Supervisor 使用内部子命令运行：

```text
herdr-pal supervise -config /path/to/config.json
```

该命令不列入普通用户帮助，职责如下：

- 持有当前规范化 Herdr Socket 对应的运行实例锁。
- 创建只允许当前用户访问的本地健康控制端点。
- 在 Worker 启动前验证 Herdr 公共 API 已经可用。
- 启动 Worker，观察其退出状态并按策略重启。
- 独立探测 Herdr Server 存活状态。
- 确认 Herdr 退出后终止 Worker，清理控制端点并释放实例锁。

### 5.3 Worker

Worker 使用内部子命令运行：

```text
herdr-pal worker -config /path/to/config.json
```

Worker 复用当前 Relay 客户端实现，但不再获取外层运行实例锁，因为该锁已经由
Supervisor 持有。Worker 仍然负责：

- 加载客户端配置和密钥。
- 维护 HPRP 长连接。
- 维护 Herdr snapshot、事件订阅和 Agent 状态。
- 处理命令、读取终端、渲染图片并发送状态通知。

当前不带子命令的 `herdr-pal` 入口继续保留手工运行语义，并自行持有实例锁，便于排错和
兼容已有部署。

## 6. 本地运行身份和控制端点

运行身份继续以规范化后的 Herdr Socket 为基础。建议在用户缓存目录中使用如下结构：

```text
<user-cache>/herdr-pal/supervisor/<socket-hash>/
├── start.lock
├── owner.lock
├── control.sock
└── supervisor.pid
```

- 目录权限为 `0700`。
- `start.lock` 只由 Launcher 短暂持有，用于串行化检查和拉起过程。
- `owner.lock` 在 Supervisor 整个生命周期内持有。
- `control.sock` 只提供健康查询，不接受业务输入和终端操作。
- `supervisor.pid` 只用于诊断，不能作为 Herdr 或 Supervisor 存活的唯一依据。

本地健康端点使用一问一答的 NDJSON，第一阶段只需要 `status`：

```json
{"method":"status"}
{"state":"running","worker_pid":1234,"herdr":"healthy"}
```

响应不得包含 Relay Key、配置内容、终端内容或完整 Socket 路径。可返回的 Supervisor
状态包括：

- `starting`：正在验证 Herdr 或启动 Worker。
- `running`：Worker 已运行，Herdr 可连接。
- `herdr_grace`：Herdr 暂时不可连接，正在退出宽限期内探测。
- `worker_backoff`：Herdr 存活，但 Worker 正在等待重启。
- `stopping`：正在清理和退出。

## 7. 启动与竞争处理

### 7.1 正常启动

```text
startup 调用 Launcher
    → 获取 start.lock
    → owner.lock 未持有且健康端点不存在
    → 启动 Supervisor
    → Supervisor 获取 owner.lock
    → ping + session.snapshot 成功
    → 启动 Worker
    → 健康端点进入 running
    → Launcher 返回成功
```

Launcher 只有在 Supervisor 已经响应健康查询，并且初始 Herdr 探测成功后才报告成功。

### 7.2 重复 startup

如果 `owner.lock` 已被持有：

- `running` 或 `worker_backoff`：已有 Supervisor 接管生命周期，Launcher 返回成功。
- `starting`：等待其完成启动，不能启动第二个实例。
- `herdr_grace`：等待现有实例恢复或释放锁，避免 handoff 结束后两个实例都退出。
- 控制端点无响应：有限等待后返回明确错误，不绕过锁并行启动。

如果锁未持有但遗留了旧控制文件，可以在当前用户私有目录内安全清理后重新启动。

## 8. Herdr 存活和退出判定

Supervisor 使用 Herdr 公共 `ping` 作为低成本探测，必要时再通过 `session.snapshot` 验证
完整可用性。不能只判断 Socket 文件存在，也不能观察某个 Herdr 或 TUI 父进程 PID。

### 8.1 状态机

```text
starting
    │ ping 和 snapshot 成功
    ▼
running
    │ ping 无法建立连接
    ▼
herdr_grace
    ├─ 宽限期内恢复 ───────────────→ running
    └─ 宽限期内持续不可连接 ───────→ stopping → exited
```

建议参数：

- 正常探测间隔：2 秒。
- 单次探测超时：1 秒。
- 断线后的快速重试间隔：500 毫秒。
- Herdr 退出宽限期：5 秒。

这些值第一阶段作为内部常量，等真实运行数据表明需要调节时再加入配置。

### 8.2 探测结果分类

- `ping` 成功且协议兼容：Herdr 存活且可供 Worker 使用。
- `ping` 返回协议不匹配：Herdr 仍然存活，Supervisor 不退出；Worker 按现有慢探测逻辑
  等待兼容版本。
- Socket 无法连接或请求超时：进入或继续 `herdr_grace`。
- 单次协议解析错误：记录错误并重试，只有持续无法获得有效响应才结束。

关闭一个 TUI、detach 或 pane 全部关闭都不影响该状态机。只有 Herdr Server 公共 API
在整个宽限期内持续不可连接，才确认 Herdr 已退出。

## 9. Worker 重启策略

Supervisor 只在 Herdr 仍然存活时重启异常退出的 Worker：

```text
Worker 退出
    ├─ Supervisor 正在 stopping：不重启
    ├─ Herdr 处于 herdr_grace：等待 Herdr 判定结果
    └─ Herdr 存活：进入 worker_backoff 后重启
```

建议使用 1、2、4、8、16、30 秒的有上限指数退避，并在 Worker 连续稳定运行 30 秒后重置。
配置错误和持续认证错误也必须退避，不能形成高频进程循环。每次退出、重启次数、退出码和
下一次重试延迟都写入结构化日志，但不得记录 Relay Key。

Supervisor 收到退出信号或确认 Herdr 退出时：

1. 向 Worker 发送 `SIGTERM`。
2. 等待当前 Pal 已有的优雅关闭窗口。
3. 超时后只终止自己创建的 Worker。
4. 清理控制 Socket、PID 文件和锁。

## 10. Live-handoff

Live-handoff 会短暂关闭旧公共 Socket，新 Server 就绪后还会再次执行插件 startup。

处理规则如下：

1. 旧 Supervisor 在第一次探测失败后进入 `herdr_grace`，不立即终止 Worker。
2. 新 Server 执行 startup 后，新 Launcher 发现 `owner.lock` 和 `herdr_grace` 状态。
3. Launcher 等待旧 Supervisor恢复或退出，不并行创建新实例。
4. 同一路径 Socket 在 5 秒内恢复时，旧 Supervisor 和 Worker继续运行。
5. 如果宽限期结束后旧 Supervisor 退出，等待中的 Launcher 获取锁并创建新 Supervisor。

这要求 Launcher 在 `herdr_grace` 状态下不能简单返回成功，否则可能出现 startup 已返回、
旧 Supervisor 随后退出而无人重新启动的竞争窗口。

## 11. 日志与诊断

脱离插件进程后，Launcher、Supervisor 和 Worker 使用同一份用户级日志文件。建议路径：

- macOS：`~/Library/Logs/herdr-pal/herdr-pal.log`
- Linux：`${XDG_STATE_HOME:-~/.local/state}/herdr-pal/herdr-pal.log`

至少记录：

- startup 触发、实例哈希和重复启动处理结果。
- Supervisor 状态变化和 Worker PID。
- Worker 退出码、重启次数和退避时间。
- Herdr 探测失败类型、宽限期开始、恢复和最终退出。
- 控制端点创建、异常和清理结果。

日志只记录 Socket 哈希，不记录完整 Socket 路径；继续沿用当前 Relay Key 脱敏规则。第一阶段
应实现有限大小的日志轮转，避免后台运行长期占满用户目录。

可后续增加只读诊断命令：

```text
herdr-pal status
```

该命令读取本地健康端点，不直接访问 Worker 业务状态。

## 12. 插件与安装器

插件目录由一体化安装包部署到用户目录，包含：

```text
herdr-pal-autostart/
├── herdr-plugin.toml
└── start-herdr-pal
```

`herdr-plugin.toml` 只声明 startup：

```toml
id = "herdr-pal.autostart"
name = "Herdr Pal Autostart"
version = "0.1.0"
min_herdr_version = "0.7.5"
platforms = ["linux", "macos"]

[[startup]]
command = ["./start-herdr-pal"]
```

`start-herdr-pal` 使用安装时写入的 Pal 绝对路径执行 `herdr-pal start`，不依赖 shell 的
`PATH`，也不保存或传递 Relay Key。

安装器需要：

- 安装插件目录并执行公开的 `herdr plugin link`。
- 保持客户端 `config.json` 的现有位置和权限。
- 从受管配置中移除安装器生成的 `[[sidecar]]` 块，但不删除用户自行配置的其他 Sidecar。
- 在修改前继续备份配置和旧插件目录。
- 检测到 Herdr 正在运行时，通过 live-handoff 加载新 startup 插件。

Sidecar 模式在代码层面暂时保留：旧配置直接执行不带子命令的 `herdr-pal` 时仍可运行。
新安装器直接使用 startup-supervisor，不向用户提供 Sidecar 与插件模式的二选一，也不再
生成受管 `[[sidecar]]` 配置。

## 13. 模块边界

建议新增 `internal/lifecycle`，避免把进程管理混入现有 Bridge：

- `Launcher`：启动串行化、现有实例检查和后台进程创建。
- `Supervisor`：Herdr 生命周期状态机和 Worker 重启。
- `HerdrProbe`：只使用公共 `ping`、`session.snapshot` 完成存活判断。
- `WorkerProcess`：启动、等待和终止子进程。
- `ControlServer` / `ControlClient`：本地只读健康协议。
- `RuntimePaths`：锁、控制 Socket、PID 和日志路径。

现有模块保持原职责：

- `bridge.Supervisor` 继续负责会话快照和订阅，不负责操作系统子进程。
- `relayclient.Client` 继续负责 HPRP 重连，不感知 Herdr 生命周期所有权。
- `processlock` 只提供锁原语，不承担健康检查。
- `installer` 负责插件和配置的原子安装，不启动长期 Worker。

## 14. 错误处理

- Launcher 无法解析 Socket：返回非零并输出明确原因。
- Launcher 发现健康实例：返回成功，保证 startup 幂等。
- owner 锁存在但健康端点持续无响应：返回错误，不启动第二个实例。
- Supervisor 初始 Herdr 探测失败：在启动窗口内重试；仍失败则清理并退出。
- Worker 配置无效：记录完整的非敏感配置错误，按慢退避重试。
- Relay 持续不可连接：由 Worker 内部重连，不重启 Worker，也不影响 Herdr 退出判断。
- Herdr 协议不匹配：Supervisor 保持存活，Worker 继续现有协议慢探测。
- 日志文件不可写：Launcher 报错并拒绝静默后台化。

## 15. 测试策略

新增逻辑优先通过接口注入时钟、进程和探测器，在本机模拟环境完成测试。

### 15.1 单元测试

- Launcher 首次启动、重复启动和并发启动。
- 锁持有但控制端点不存在、无响应或状态异常。
- Supervisor 正常启动和 Worker 退出重启。
- Worker 稳定窗口后的退避重置。
- Herdr 短于 5 秒的断线恢复不退出。
- Herdr 持续断线超过宽限期后终止 Worker。
- 协议不匹配证明 Herdr 存活，不触发退出。
- live-handoff 中新 Launcher 等待旧 Supervisor恢复或释放锁。
- 信号退出、Worker 超时终止和运行文件清理。
- 控制协议拒绝未知方法和畸形 NDJSON。
- 插件目录、绝对启动路径和重复安装的幂等性。
- 从受管 Sidecar 配置迁移时不删除用户自定义 Sidecar。

### 15.2 本地集成测试

- 使用假的 Unix Socket Herdr Server 模拟 `ping` 和 `session.snapshot`。
- 重启同一路径 listener，验证 handoff 宽限期。
- 使用可控退出的假 Worker 验证 Supervisor 进程重启。
- 在没有 IM 网络的情况下启动真实 Pal Worker，Relay 使用本地 fake server。
- 执行两次插件 startup，确认只存在一个 Supervisor 和一个 Worker。

### 15.3 真实 Herdr 验证

- 正常启动 Herdr 后 Pal 自动上线并出现在 `/ls`。
- `herdr server live-handoff` 不产生重复 Pal 实例。
- 结束 Worker 后 Supervisor自动重启并恢复 HPRP 上报。
- detach TUI 不结束 Pal。
- `herdr server stop` 后 Pal 在宽限期结束后退出。
- 再次启动 Herdr 后 Pal 自动启动。

## 16. 实施顺序

1. 提取 Worker 的“是否获取实例锁”运行边界，保持现有默认入口行为不变。
2. 实现 `RuntimePaths`、控制协议和 Launcher 幂等启动。
3. 实现 HerdrProbe 和 Supervisor 生命周期状态机。
4. 实现 Worker 子进程重启、信号转发和日志重定向。
5. 增加插件包，把安装器默认启动机制切换到 startup-supervisor，并迁移受管 Sidecar 配置。
6. 完成本地 fake Herdr、fake Relay 和真实 Herdr 验证。

## 17. 验收标准

- 不修改 Herdr 源码即可在 Herdr 启动后自动拉起 Pal。
- startup 命令在 5 秒内成功返回或输出可诊断错误。
- 重复 startup、并发 startup 和 live-handoff 均不产生重复实例。
- Worker 异常退出且 Herdr 存活时能够按退避策略自动恢复。
- Herdr 短暂断线不会中止 Pal；持续不可连接超过宽限期后 Pal 自动退出。
- Relay 断线不会导致 Supervisor误判 Herdr 退出。
- 所有新增单元测试可由根目录 `unittest.sh` 运行，完整构建可由 `build.sh` 完成。
