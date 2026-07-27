# Windows AMD64 支持设计

## 目标

为与 Herdr 原生 Windows Beta 配套运行的 `herdr-pal` 客户端增加 Windows AMD64 支持，
保持单文件分发、Relay 协议和 Herdr protocol 17 业务逻辑不变。

首期支持范围：

- 发布 `herdr-pal-windows-amd64.exe`。
- 支持默认会话、命名会话和显式 `socket_path`。
- 支持普通请求、长连接事件订阅、本机交互模式和 Relay 客户端模式。
- Windows 状态标记为 Beta，与 Herdr Windows 的支持级别一致。

本次不把 Windows 版 `herdr-pal-server` 作为发布目标；服务端仍推荐部署在 Linux。

## 平台传输

Herdr 在 Unix 使用 Unix Domain Socket，在 Windows 使用 Named Pipe，但两者承载相同的
NDJSON protocol 17。`HerdrClient` 保留统一的请求、响应和订阅逻辑，只把本地连接建立过程
抽象为平台拨号器。

- Unix：使用 `net.Dialer` 连接 `unix` 地址。
- Windows：使用 `go-winio` 连接 Named Pipe。
- Herdr CLI 返回的 Windows `socket`/`socket_path` 是 marker 文件路径。Herdr 使用
  `GenericNamespaced` 将该字符串映射到 `\\.\pipe\<marker path>`，Pal 必须采用相同映射。
- Windows 不解析 marker 路径中的 symlink/junction，因为 Named Pipe 名称取决于路径字符串；
  Unix 仍解析 symlink 以冻结 Socket 身份。
- 测试注入的自定义拨号器继续接收原始 endpoint，避免破坏现有协议单元测试。

## 路径与探测

Socket 解析顺序保持“显式路径、Herdr CLI、平台默认路径”。Windows 默认路径根据 Herdr
规则从 `XDG_CONFIG_HOME`、`APPDATA`、`USERPROFILE`、`HOME` 依次推导，并通过 Named Pipe
连接检查有效性。

Unix 的 Socket 长路径短别名只在 Unix 生效。Windows marker 路径直接用于锁身份和 Named
Pipe 名称，不创建 `/tmp` 目录或 symlink。

## 构建与验证

- `build.sh` 增加 `herdr-pal-windows-amd64.exe`，并纳入 `SHA256SUMS`。
- README 补充 Windows Beta 产物、配置路径和启动示例。
- 单元测试覆盖 Windows pipe 名称映射、平台 HOME 路径推导、Unix 行为保持以及构建矩阵。
- 本机执行全部单元测试、竞态测试和构建；同时交叉编译 Windows AMD64 客户端及测试包。
