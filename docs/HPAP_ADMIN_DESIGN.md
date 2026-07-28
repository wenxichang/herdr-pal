# Herdr Pal 本地管理面设计

## 1. 文档状态

本文定义 `hp-cli` 与 `herdr-pal-server` 之间的本地管理面。参考实现已经完成 HPAP/1、
`hp-cli`、动态凭据管理、运行查询和服务生命周期接入，并通过本地端到端与竞态测试。

本地管理协议命名为 Herdr Pal Administration Protocol，首个版本为 `HPAP/1`。HPAP 只在
Server 主机的 Unix Domain Socket 上使用，与 Pal/Server 远程通讯所使用的 HPRP/1 完全
隔离。

## 2. 目标与非目标

### 2.1 目标

- 所有 Key 签发和变更都由运行中的 Server 进程完成，并立即作用于认证内存。
- 提供独立的 `hp-cli` 管理工具，通过本机 Unix Socket 查询和控制 Server。
- 支持 Key 的签发、查询、启用、禁用、删除和来源地址策略管理。
- 支持查询 Server、企业微信、HPRP 连接和在线 Agent 会话的运行信息。
- 支持主动断开 Pal、切换详细日志和请求 Server 优雅退出。
- 使用文件权限和操作系统 peer UID 建立本机管理权限边界。
- 查询命令同时适合人工查看和脚本处理。

### 2.2 非目标

- 不提供远程 TCP 管理端口、浏览器管理后台或公网管理 API。
- 不通过 HPAP 发送 Agent prompt、按键或其他 Herdr 操作。
- 不读取 `/con`、终端内容、Cookie、Bot Secret 或 Key Secret。
- 不支持热加载普通配置、替换 TLS 证书或切换企业微信机器人。
- 不支持多 Server 实例发现、集群管理或跨主机聚合。
- 首版不支持 Windows Server 或 Windows `hp-cli`。
- 凭据文件使用 version 2，不兼容实施前没有来源规则的旧记录。

## 3. 总体架构

```text
hp-cli
  │ HPAP/1 NDJSON
  │ Unix Socket
  ▼
AdminServer
  ├── RuntimeInspector ── Server / WeCom / TLS / 日志状态
  ├── CredentialManager ─ CredentialStore
  ├── ConnectionManager ─ ClientHub
  └── SessionInspector ── SessionCatalog
```

`AdminServer` 是 `herdr-pal-server` 的内部运行组件，与企业微信连接、HPRP Listener 共用
生命周期。它只依赖窄接口，不直接持有或操作所有 Server 内部对象。

默认管理 Socket 位于：

```text
<state_dir>/admin.sock
```

`hp-cli` 默认读取 `~/.config/herdr-pal/server-config.json`，使用与 Server 相同的配置加载
逻辑推导 `state_dir` 和 Socket 路径。保留 `-config` 用于 Server 使用非默认配置路径的
部署，但不设计自动发现多个实例。

Admin Socket 创建失败时，Server 启动失败，避免出现无法管理的半可用状态。Server 正常
退出时删除 Socket 文件。

## 4. 本机安全边界

- `state_dir` 权限必须为 `0700`。
- Admin Socket 权限必须为 `0600`。
- Server 启动时使用 `lstat` 检查现有路径；符号链接、普通文件和其他非 Socket 类型一律
  拒绝，不能直接删除。
- 对陈旧 Socket，Server 必须先尝试连接；只有确认没有进程监听时才删除并重新创建。
- Linux 使用 peer credentials，macOS 使用等价的 peer UID 查询；连接方 UID 必须等于
  Server 进程 UID。
- HPAP 不再增加 Token、密码或 Cookie 认证。
- `hp-cli` 不能直接打开或修改凭据文件。
- 每个管理操作都写入结构化审计日志，但不记录明文 Token、Bot Secret 或终端内容。

## 5. HPAP/1 协议

### 5.1 传输

- 使用 Unix Domain Socket 字节流。
- 编码为 UTF-8 NDJSON，每行一个完整 JSON object。
- 一个连接可以顺序发送多个请求；同一连接内按请求顺序处理。
- 多个 `hp-cli` 连接可以并发，Server 必须限制连接数和单连接待处理请求数。
- 单帧硬限制为 1 MiB，超过限制立即关闭连接。
- 默认连接、读取和写入超时为 5 秒；`server.stop` 仍必须先写回响应。
- 重复 JSON 字段、尾随内容、非法 UTF-8 和不支持的协议版本必须明确拒绝。

### 5.2 请求与响应

请求信封：

```json
{
  "protocol": "HPAP/1",
  "id": "req-1",
  "method": "connection.disconnect",
  "params": {
    "connection_id": "conn-01J..."
  }
}
```

成功响应：

```json
{
  "protocol": "HPAP/1",
  "id": "req-1",
  "result": {
    "observed_at": "2026-07-28T12:00:00Z",
    "connection_id": "conn-01J...",
    "disconnected": true
  }
}
```

失败响应：

```json
{
  "protocol": "HPAP/1",
  "id": "req-1",
  "error": {
    "code": "credential.not_found",
    "message": "Key 不存在",
    "details": {}
  }
}
```

`id` 只用于当前连接内的请求关联。`result` 与 `error` 必须且只能出现一个。`message` 仅用于
人工展示，程序根据稳定 `code` 决策。

### 5.3 错误码

首版稳定错误码：

```text
protocol.invalid_request
protocol.unsupported_version
protocol.unsupported_method
protocol.limit_exceeded
argument.invalid
credential.not_found
credential.conflict
credential.source_required
credential.source_invalid
connection.not_found
server.busy
server.internal
```

业务错误不关闭连接；协议编码错误、超限帧或持续状态机违规可以关闭连接。

### 5.4 列表分页

`key.list`、`connection.list` 和 `session.list` 支持：

```json
{
  "limit": 100,
  "page_token": "opaque-token"
}
```

`limit` 默认 100，最大 500。结果返回可选 `next_page_token`。列表使用稳定排序，但运行时
目录可以同时变化，因此分页提供的是逐页观察结果，不承诺跨页事务快照。

`hp-cli` 默认自动获取全部分页后输出表格；`--json` 也聚合为一个最终 JSON 结果。未来若
数据规模需要，可以增加显式分页参数，而不改变 HPAP 方法语义。

## 6. hp-cli 命令

### 6.1 Server

```text
hp-cli server status [--json]
hp-cli server stop [--json]
hp-cli server debug enable [--json]
hp-cli server debug disable [--json]
```

`server.stop` 先发送成功响应，再触发与 SIGTERM 相同的有界优雅退出。`server debug` 使用
`slog.LevelVar` 动态切换 `debug` 与配置文件中的基础日志级别，进程重启后恢复配置值。

### 6.2 Key

```text
hp-cli key issue --principal-id USERID --machine-id MACHINE --source SOURCE... [--expires-at RFC3339] [--json]
hp-cli key list [--json]
hp-cli key show CREDENTIAL_ID [--json]
hp-cli key enable CREDENTIAL_ID [--json]
hp-cli key disable CREDENTIAL_ID [--json]
hp-cli key delete CREDENTIAL_ID --yes [--json]

hp-cli key source list CREDENTIAL_ID [--json]
hp-cli key source add CREDENTIAL_ID SOURCE... [--json]
hp-cli key source remove CREDENTIAL_ID SOURCE... [--json]
hp-cli key source set CREDENTIAL_ID SOURCE... [--json]
```

`--source` 可以重复使用。`key delete` 不可恢复，必须显式提供 `--yes`；首版不支持模糊
匹配、通配符、批量删除或按用户删除。

`--expires-at` 可选，必须使用 RFC3339 UTC 或带时区时间；留空表示永不过期。首版不提供
修改现有 Key 有效期的命令，需要变更时重新签发 Key。

### 6.3 Connection

```text
hp-cli connection list [--json]
hp-cli connection show CONNECTION_ID [--json]
hp-cli connection disconnect CONNECTION_ID [--json]
```

`connection disconnect` 只断开当前 HPRP 连接，不改变 Key。Key 仍为 enabled 时，Pal 可以
按原重连策略再次连接。

### 6.4 Session

```text
hp-cli session list [--principal-id USERID] [--machine-id MACHINE] [--json]
```

`session list` 不读取终端内容。它使用与企业微信 `/ls` 相同的当前排序和编号算法，但不能
修改用户的 `/ls` 编号缓存或当前选择。

### 6.5 退出码

```text
0  请求成功
1  Server 返回业务错误
2  命令行参数或本地配置错误
3  Admin Socket 不可用、协议不兼容或传输失败
```

## 7. Key 数据模型与生命周期

持久化记录包含：

```json
{
  "credential_id": 12,
  "principal_id": "wecom-user-id",
  "machine_id": "office-pc",
  "secret_sha256": "...",
  "status": "enabled",
  "allowed_sources": [
    "192.168.1.20",
    "192.168.1.0/24",
    "192.168.2.1-192.168.2.10"
  ],
  "expires_at": null,
  "created_at": "2026-07-28T12:00:00Z",
  "updated_at": "2026-07-28T12:00:00Z"
}
```

状态只使用 `enabled` 和 `disabled`。删除表示记录永久移除，不增加可恢复的 deleted 状态。
参考实现直接使用 version 2 凭据文件格式，不加载无来源规则的旧记录。

### 7.1 Credential ID 分配

`credential_id` 是从 1 开始的无符号十进制运行时条目标识，不是安全凭据，也不承诺全局
唯一或永久不复用。Token 格式为：

```text
hpk_<credential_id>_<secret>
```

CredentialStore 启动时扫描当前全部记录，把运行期下一 ID 初始化为“现存最大 ID + 1”；
空存储从 1 开始。运行过程中签发操作在 Store 锁内取当前值并递增，删除不会降低该计数器，
因此中间删除会留下空洞。

下一 ID 不持久化。Server 重启后重新根据现存记录计算；如果尾部 ID 已全部删除，重启后
可以回收这些尾部数值。只要仍有更大 ID 存在，中间空洞不会被主动填补。计数器溢出时拒绝
继续签发。

### 7.2 Issue

- 必须提供 principal ID、machine ID 和至少一条来源规则。
- 新记录默认 enabled。
- Secret 至少包含 256 位安全随机熵。
- 可选过期时间必须晚于当前时间；留空表示永不过期。
- 服务端只持久化摘要；完整 `hpk_...` Token 只在本次响应返回。
- 如果持久化失败，不更新认证内存，也不返回 Token。

### 7.3 Enable 与 Disable

- `enable` 将状态持久化为 enabled；不会主动要求 Pal 建立连接。
- `disable` 将状态持久化为 disabled，然后按 credential ID 立即撤下并断开活动连接。
- 认证读取与状态变更由同一 Store 锁保护；持久化失败时保持原状态并且不主动断连。
- 已过期 Key 即使 enabled 也不能认证。

### 7.4 Delete

- 删除操作先原子持久化不包含该记录的新凭据文件，再从内存删除记录。
- 持久化成功后按 credential ID 撤下并断开活动连接。
- 持久化失败时记录和连接保持原状态。
- 被删除的记录本身不可查询、启用或恢复；Server 重启后可能把已释放的尾部数值分配给
  全新的 Key。

### 7.5 多 Key 与轮换

同一 `(principal_id, machine_id)` 可以存在多把 Key，用于轮换。Server 仍只允许该用户和
机器同时存在一条 HPRP 连接。连接记录保存实际使用的 credential ID，因此禁用某把 Key
只断开使用该 Key 的连接。

## 8. 来源地址策略

### 8.1 支持格式

每条规则支持：

```text
单地址：192.168.1.20
CIDR：192.168.1.0/24
闭区间：192.168.1.1-192.168.1.5
```

IPv4 和 IPv6 都支持三种格式。

- IP 使用标准规范形式保存。
- CIDR 使用掩码后的网络地址保存。
- 范围两端必须属于同一地址族，起始地址不能大于结束地址。
- 重复规则去重，重叠规则保留，不自动合并。
- `source remove` 按规范化后的完整规则删除；删除后仍被其他重叠规则覆盖属于正常行为。
- `source remove/set` 不能产生空规则列表。
- 确实需要不限来源时必须显式配置 `0.0.0.0/0` 和 `::/0`。

### 8.2 认证

Server 从 TLS 请求的 TCP peer 地址取得来源 IP，规范化后与 Key 的规则匹配。禁止信任
`X-Forwarded-For`、`Forwarded` 或其他代理头。

IPv4-mapped IPv6 地址在匹配前使用等价 IPv4 地址规范化，避免同一来源因操作系统表示差异
绕过或误拒绝 IPv4 规则。

Bearer Secret、状态、过期时间和来源地址必须全部通过，才能建立 HPRP 连接。任一失败都
统一返回 HTTP 401，不能向 Pal 暴露具体失败原因。

活动连接保存建立连接时的规范化来源 IP，用于管理查询和规则变更后的复核。

### 8.3 动态变更

- `source add` 持久化后立即影响后续认证，不需要断开现有连接。
- `source remove/set` 持久化后重新检查当前连接。
- 当前来源不再符合新规则时，Server 立即撤下会话并断开连接。
- 当前来源仍符合规则时，连接保持不变。

## 9. 运行信息

### 9.1 server status

返回：

- Server 版本、PID、启动时间、运行时长、OS 和架构。
- HPAP 与 HPRP 主版本。
- 企业微信状态：connected、reconnecting、stopped，最近变化时间和最近安全错误类别。
- Relay 监听地址与 Admin Socket 路径。
- TLS 模式、证书到期时间和不含私钥的证书指纹。
- 当前 debug 状态与基础日志级别。
- 当前 principal 数、机器连接数和 Agent 会话数。
- enabled、disabled 和过期 Key 数量。

错误信息只能返回安全错误类型和稳定错误码，不返回 Secret、Cookie 或完整底层响应。

### 9.2 connection list/show

返回：

- connection ID、credential ID、完整 principal ID 和 machine ID。
- Pal 版本、OS 和架构。
- 规范化来源 IP、连接时间、最近 heartbeat 时间。
- 最近快照时间、快照序号和会话数。
- 已协商 capability。

### 9.3 session list

每项返回：

- principal ID、机器标识和当前用户内编号。
- Workspace/Tab、Agent、展示 Agent、pane 和标题。
- `done ✅`、`working ⏳`、`blocked ⁉️`、`idle 💤` 或 `unknown ❔`。
- JSON 输出包含完整 HPRP `machine_id + slot_id + session_id` 稳定目标。

## 10. 原子性、并发与背压

- CredentialStore 是 Key 持久化和认证内存的唯一所有者。
- 所有变更在进程内串行化：校验新状态、写入权限受限的临时文件、`fsync`、原子重命名，
  然后提交内存状态。原子重命名是磁盘与内存的共同提交点；其后的目录 `fsync` 只增强崩溃
  耐久性，失败时不能把已经替换的磁盘文件伪装为回滚。
- 持久化期间认证读取可以短暂等待，不能观察到文件与内存不一致的中间状态。
- AdminServer 使用有界连接数和响应大小；同一连接顺序处理请求，不为每个请求创建无界
  goroutine。
- Runtime、Connection 和 Session 查询各自返回线程安全快照，不承诺跨组件全局事务一致性；
  每个结果包含 `observed_at`。
- 断连动作先从 Hub 的可路由集合撤下连接，再取消连接 context，避免继续接受新命令。
- `server.stop` 将响应完整写出后再触发 Server 根停止入口；响应写入失败时释放停止预留，
  允许管理员重新请求停止。

## 11. 代码边界

- `cmd/hp-cli`：命令行解析、表格/JSON 输出和退出码。
- `internal/adminproto`：HPAP/1 信封、方法、结果、错误和严格编解码。
- `internal/adminclient`：Unix Socket 连接、超时、分页和请求关联。
- `internal/adminserver`：Socket 安全、peer UID、方法分发、背压和审计。
- `internal/credential`：Key CRUD、来源规则和原子文件存储。
- `internal/server.ClientHub`：连接快照、按 connection/credential 撤下和断连。
- `internal/server.SessionCatalog`：不改变路由状态的管理只读快照。
- `internal/wecom.Client`：线程安全的连接状态快照。
- `internal/serverapp`：组件装配、运行元数据、动态日志级别和优雅停止。

AdminServer 通过 `RuntimeInspector`、`CredentialManager`、`ConnectionManager` 和
`SessionInspector` 接口访问能力，不能成为直接依赖所有内部类型的 god object。

## 12. 审计日志

每个 HPAP 请求记录：

- peer UID。
- method。
- request ID 摘要。
- 目标 credential ID、connection ID 或筛选条件；principal ID 只记录摘要。
- 成功或稳定错误类型。
- 是否断开连接、受影响数量和耗时。

Key 签发成功只记录 credential ID。任何日志都不能包含完整 Token、Secret SHA-256、Bot
Secret、prompt 或终端快照。

HPRP 来源认证失败可以记录 credential ID、来源 IP 和安全失败类别，但对客户端仍统一返回
HTTP 401。

## 13. 测试策略

### 13.1 单元测试

- HPAP 严格 JSON、重复字段、未知协议、未知方法、错误关联和超限帧。
- Socket 路径类型、权限、陈旧 Socket、安全清理和 peer UID。
- Key issue/list/show/enable/disable/delete 的持久化、回滚和并发验证。
- 明文 Token 不进入凭据文件、日志和查询结果。
- IPv4/IPv6 单 IP、CIDR、闭区间及边界地址匹配。
- 非法 IP、非法 CIDR、反向范围、跨地址族范围、重复规则和空规则。
- source add/remove/set 的规范化、持久化和活动连接复核。
- Server、WeCom、TLS、连接和会话运行快照。
- session list 排序、编号、展示字段和 emoji 与 `/ls` 一致，且不改变路由缓存。
- `server.stop` 响应先于根 context 取消。
- 动态日志级别切换和重启恢复基础级别。

### 13.2 集成测试

使用本地 Unix Socket、fake WeCom 和 fake HPRP Pal 验证：

- hp-cli 签发 Key 后 Pal 无需重启 Server 即可连接。
- 不符合来源规则的 Pal 收到 HTTP 401。
- disable、delete 和收紧来源规则立即撤下机器和全部会话。
- enable 后 Pal 的自动重连可以成功。
- connection disconnect 不禁用 Key，Pal 可以再次连接。
- session list 返回多个用户、机器和状态，且不访问终端输出。
- 多个 hp-cli 并发查询和串行 Key 变更不会丢失记录。
- Server 停止时 Admin Socket、HPRP、企业微信和凭据文件正确收尾。

## 14. 构建产物

`build.sh` 增加：

```text
dist/hp-cli-darwin-amd64
dist/hp-cli-darwin-arm64
dist/hp-cli-linux-amd64
dist/hp-cli-linux-arm64
```

当前平台同时生成 `dist/hp-cli`。Windows 首版不生成 hp-cli，也不发布 Server 管理面。

## 15. 实施结果与边界

- HPAP 信封、严格编解码、稳定错误码、分页和来源规则已经实现。
- CredentialStore 是动态 Key CRUD 的唯一写入口；Server 运行期间签发和变更立即生效。
- Admin Socket 会校验目录、现有路径、文件权限和 peer UID；派生路径超过 Unix Socket 安全
  长度时在配置阶段拒绝启动。
- `hp-cli` 支持人工表格和聚合后的 `--json` 输出；人工输出会替换控制字符，避免在线会话
  标题或状态污染本地终端。
- `session list` 只读取 SessionCatalog 管理快照，不读取终端内容，也不改变企业微信路由、
  `/ls` 编号缓存或当前选择。
- Server 停止时先撤下 HPRP 路由，再断开连接，并等待已经 Upgrade 的 WebSocket handler
  退出，避免 HTTP `Shutdown` 遗留活动 Pal。
- Darwin 和 Linux 构建 Server 与 `hp-cli`；Windows 当前只提供 Pal Beta，不包含本地管理面。
