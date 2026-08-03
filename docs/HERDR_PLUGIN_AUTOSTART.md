# Herdr 插件自动启动与生命周期管理

> 本文记录 Herdr 插件能力的审核结论和当前实现。详细进程模型、状态机、安装迁移和测试设计见
> [Pal 自动启动与自守护设计](plans/2026-08-03-pal-autostart-supervisor-design.md)。

## 背景

Herdr Pal 需要在 Herdr Server 启动后自动运行，并在 Herdr Server 真正退出后自动
结束。该能力不得依赖 `launchd`、`systemd` 等系统服务，也不修改 Herdr 源码。

本文记录对 Herdr 0.7.5 插件能力的审核结论，以及 Herdr Pal 自动启动和自守护实现遵守的
生命周期规则。

## 结论

Herdr 0.7.5 提供的插件 `[[startup]]` 钩子足以作为 Herdr Pal 的启动触发器：

- 启用插件后，Herdr 在恢复会话且公共 API Socket 就绪后执行 startup 命令。
- Live handoff 完成后，新 Server 会再次执行 startup 命令。
- startup 命令异步执行，失败不会阻止 Herdr Server 启动。
- 插件注册是当前用户级配置，不需要安装系统服务。

Herdr 没有 sidecar 守护器，也没有插件 shutdown hook。startup 钩子被定义为一次性
初始化命令，不应直接承载长期运行的 Herdr Pal 进程。因此，Herdr Pal 需要提供一个
短生命周期启动器和一个自行管理的后台进程。

## 目标结构

```text
Herdr Server
    │
    │ [[startup]]，公共 Socket 已就绪
    ▼
Herdr Pal Launcher
    │ 检查单实例并启动后台进程
    │ 随后立即退出
    ▼
Herdr Pal Supervisor
    │ 监控 Herdr 和 Worker
    ▼
Herdr Pal Worker
    │ Herdr 公共 Socket
    └─ Relay 长连接
```

Launcher 必须快速退出，避免长期占用 Herdr 的插件命令槽位。Supervisor 和 Worker
由 Herdr Pal 自己实现和管理，不由 Herdr 插件运行时托管。

## 启动语义

插件通过本地目录注册，例如：

```bash
herdr plugin link ~/Code/herdr-pal/plugin
```

插件目录中的 `herdr-plugin.toml` 应声明 Herdr 0.7.5 为最低版本，并只运行
Herdr Pal 的启动器：

```toml
id = "herdr-pal.autostart"
name = "Herdr Pal Autostart"
version = "0.1.0"
min_herdr_version = "0.7.5"
platforms = ["linux", "macos"]

[[startup]]
command = ["./start-herdr-pal"]
```

插件命令是 argv 数组，不进行 Shell 展开。路径、环境变量、日志重定向和后台化逻辑
应由启动器处理。

## Herdr 退出判定

单条订阅连接断开不能直接证明 Herdr Server 已退出。状态订阅可能因为 pane 或 Agent
变化而重建，连接也可能因临时错误结束。

当前 Supervisor 每 2 秒主动探测公共 `ping`，断连后每 500 毫秒重试，并以公共 Socket
持续不可连接作为最终退出判据：

```text
定时探测同一 HERDR_SOCKET_PATH
    │
    ├─ 5 秒内恢复：继续运行
    │
    └─ 5 秒内始终无法连接：认定 Herdr Server 已退出
                              停止 Worker 并退出 Supervisor
```

首次就绪必须完成公共协议兼容性检查和权威 snapshot；后续存活探测使用 `ping`，不能只判断
Socket 文件是否存在。Server 崩溃后可能残留 Socket 文件，但连接会失败。

这里的退出对象是 Herdr Server，而不是某个 TUI 客户端：

- 用户 detach 或关闭一个 Herdr 客户端，但 Server 仍运行时，Herdr Pal 继续运行。
- `herdr server stop`、Server 崩溃或 Server 真正结束时，Herdr Pal 在探测窗口结束后
  自动退出。

## Live handoff

Live handoff 会关闭旧 Server 的公共 Socket，再由新 Server 监听同一路径。新 Server
准备完成后还会再次执行插件 startup 钩子。

Herdr Pal 必须同时处理两个条件：

1. Supervisor 在短暂断线期间继续探测，Socket 在 5 秒内恢复时保持运行。
2. 新 startup 调用发现已有 Supervisor 持有单实例锁时返回成功，不启动重复实例。

因此，正常 handoff 不应导致 Relay 长连接和当前会话状态被主动重建。若 handoff 超出
探测窗口并导致旧 Supervisor 退出，新 startup 启动器必须能够在旧锁释放后重新启动
Supervisor，避免出现两个进程都退出的竞争窗口。

## 自守护规则

Supervisor 只在确认 Herdr Server 仍可连接时重启异常退出的 Worker：

```text
Worker 异常退出
    │
    ├─ Herdr 可连接：按退避策略重新启动 Worker
    │
    └─ Herdr 不可连接：进入 Herdr 退出探测，不再盲目重启
```

重启应采用有上限的指数退避，避免配置错误或持续故障形成高频进程循环。Supervisor
退出时应停止 Worker、等待必要的清理完成，并释放进程锁。

## 单实例与竞争处理

Herdr Pal 当前已经按照规范化后的 Herdr Socket 身份获取进程锁。自动启动模式还应
保证同一 Socket 只存在一个 Supervisor。

启动器遇到锁冲突时不能把它视为 Herdr 启动失败：

- 已有 Supervisor 健康运行时，启动器返回成功。
- 已有 Supervisor 正在退出时，启动器应在有限时间内等待锁释放并再次尝试。
- 等待结束后仍无法确认健康实例时，记录明确错误并退出，不能并行启动第二个实例。

不同 Herdr Socket 可以各自运行实例，但当前产品约束仍是一台机器只运行一个 Herdr。

## 日志与配置

- Launcher、Supervisor 和 Worker 使用同一套结构化日志约定。
- 后台进程的 stdout 和 stderr 不得继续连接 Herdr 插件命令的输出管道。
- 日志写入 Herdr Pal 自己的用户日志目录，不记录 Relay 用户凭据或完整终端内容。
- 插件启动时可以使用 Herdr 注入的 `HERDR_SOCKET_PATH`，但用户配置中的显式 Socket
  选择仍具有稳定、可审计的优先级。
- 启动失败、Worker 重启、Herdr 断线探测和最终退出都必须有明确日志。
- macOS 日志位于 `~/Library/Logs/herdr-pal/herdr-pal.log`；Linux 默认位于
  `~/.local/state/herdr-pal/herdr-pal.log`，设置 `XDG_STATE_HOME` 时随其变化。

## 不采用的方案

### 直接将 Herdr Pal 作为 startup 命令长期运行

该方式虽然能启动进程，但会长期占用插件命令槽位，且 Herdr 不提供进程重启和关闭
管理，不作为生产方案。

### 只监控父进程 PID

Live handoff 会更换 Herdr Server 进程。仅根据父 PID 退出会把正常 handoff 误判为
Server 结束，也会引入新旧启动器争抢单实例锁的问题。

### 安装系统服务

本方案不依赖 `launchd`、`systemd` 或 Windows Service。所有自动启动和守护逻辑都
随 Herdr Pal 与本地插件目录分发。

## 验收标准

- 启动 Herdr Server 后，Herdr Pal 自动启动且 startup 命令能及时结束。
- 重复 startup 和 Live handoff 不产生重复 Herdr Pal 实例。
- Worker 异常退出且 Herdr 仍运行时能够自动恢复。
- Herdr Server 短暂切换或 handoff 时 Herdr Pal 保持运行。
- Herdr Server 持续不可连接超过探测窗口后 Herdr Pal 自动退出。
- TUI detach 不会导致 Herdr Pal 退出。
- 下次启动 Herdr Server 时 Herdr Pal 能再次自动启动。
- 全流程不需要安装或启用任何操作系统服务。
