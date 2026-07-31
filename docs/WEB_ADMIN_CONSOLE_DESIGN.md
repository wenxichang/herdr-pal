# Herdr Pal Web 管理台设计

## 1. 目标

Herdr Pal Server 在保留现有 HPAP/1 Unix Socket 和 `hp-cli` 的基础上，内嵌一套仅使用
HTTPS 的 Web 管理台。管理台覆盖现有 `hp-cli` 的运行管理能力，并增加管理员账户、外部
自动化凭据签发接口和 Loki 审计日志查询。

本设计遵循以下原则：

- `hp-cli` 与 Web 管理台共享同一套业务规则，不能形成两套实现。
- Web 页面、CSS 和 JavaScript 编入 `herdr-pal-server`，继续保持单文件分发。
- 管理平面不接触 Herdr 原始 Socket，不提供通用 Herdr RPC，也不能发送 Agent 输入。
- 管理操作只写普通 Server HTTP 日志，不进入用户内容审计流。
- 密码、机器 Key、自动化 Token、Cookie 和 CSRF Token 均不得以明文持久化。

## 2. 范围

Web 管理台提供：

- Server 状态、动态 debug 开关和优雅停止。
- 机器凭据签发、查看、启用、禁用、删除和来源地址维护。
- 在线连接列表、连接详情和主动断开。
- 当前全部用户、机器和 Agent 会话列表，状态信息与用户 `/ls` 对齐。
- Loki 用户输入和终端文本审计查询。
- 多管理员账户管理、密码重置和自动化 Token 管理。
- 供外部 IT 系统使用的机器凭据签发、删除 API。

以下内容不在本次范围内：

- 修改 HPRP/1 或 HPAP/1 协议语义。
- 替换 `hp-cli`，或让 Web 管理台通过 HPAP 回调本机 Unix Socket。
- 在 Web 管理台发送 Agent prompt、按键或审批动作。
- 在管理台内保存、展示终端图片；审计只查询图片请求附带的文本。
- 细粒度管理员角色和租户隔离。首版所有管理员权限相同。
- 任意 LogQL 控制台。

## 3. 总体架构

```text
hp-cli ── HPAP/1 Unix Socket ──┐
                               ├── AdminService ── CredentialStore
Web 管理台 ── HTTPS/JSON API ──┤                 ├── ClientHub
                               │                 ├── SessionCatalog
IT 系统 ── HTTPS/Bearer Token ─┘                 └── Server Runtime

Web 管理台审计页 ── LokiQueryClient ── Loki HTTP API
```

现有 `AdminServer` 继续负责 HPAP/1 的传输、Unix peer UID 校验和请求解析。新增 Web
管理入口负责 HTTPS、浏览器认证、CSRF 和 JSON API。两者调用同一个 `AdminService`，由
该服务统一实施凭据冲突、最后管理员保护之外的运行管理规则、连接断开和 Server 生命周期
控制。

管理员账户不属于 HPAP/1 的管理对象，由独立 `AuthStore` 管理。Loki 查询是 Web 管理台
专属的只读能力，由 `LokiQueryClient` 承担，不进入 `AdminService`。

建议模块边界：

- `AdminService`：封装现有 hp-cli 可用的 Server、凭据、连接和会话操作。
- `WebAdminServer`：HTTPS 生命周期、路由、页面和 JSON 编解码。
- `AuthStore`：管理员、密码摘要、自动化 Token 摘要和原子持久化。
- `SessionManager`：内存登录 Session、超时和撤销。
- `LoginGuard`：按用户名和来源 IP 实施登录失败锁定。
- `LokiQueryClient`：生成受控 LogQL、调用 Loki、校验和裁剪结果。
- `WebAssets`：通过 `embed.FS` 提供模板、样式和原生 JavaScript。

## 4. 配置与启动

Server 配置增加：

```json
{
  "admin": {
    "listen": "0.0.0.0:4001",
    "loki_url": "http://127.0.0.1:3100"
  }
}
```

- `admin.listen` 缺省或留空时使用 `0.0.0.0:4001`。
- `admin.loki_url` 是独立配置，不从 `audit.endpoint` 推导。
- 管理台复用 HPRP Server 的 TLS 证书和私钥，仅提供 HTTPS。
- 管理员文件固定为当前运行用户的 `~/.config/herdr-pal-server/auth.json`。
- 管理端口监听失败、TLS 材料无效或管理员文件损坏时，整个 Server 启动失败。
- Loki 不可用不阻断 Server 启动，只使审计查询返回明确错误。

启动顺序为：

1. 加载 Server 配置和 TLS 材料。
2. 加载并校验 `auth.json`。
3. 必要时创建初始 `admin`。
4. 初始化共享 `AdminService`、HPAP Server 和 Web 管理台。
5. 所有必需监听成功后进入运行状态。

通过 Web 停止 Server 时，HTTP Handler 先完成成功响应，再触发统一的优雅关闭。关闭阶段先
停止接受新的管理请求，最多等待 5 秒让在途请求结束，然后关闭企业微信、HPRP、HPAP 和
持久化资源。

## 5. 管理员存储

`auth.json` 使用版本化结构：

```json
{
  "version": 1,
  "users": [
    {
      "username": "admin",
      "password_hash": "$argon2id$...",
      "must_change_password": true,
      "created_at": "2026-07-30T00:00:00Z",
      "updated_at": "2026-07-30T00:00:00Z",
      "automation_token": {
        "token_id": "8df1a9c2",
        "secret_digest": "sha256:...",
        "enabled": true,
        "created_at": "2026-07-30T00:00:00Z",
        "updated_at": "2026-07-30T00:00:00Z"
      }
    }
  ]
}
```

存储规则：

- 用户名统一规范化为小写，并匹配 `[a-z][a-z0-9._-]{2,31}`。
- 密码长度 12 至 256。Argon2id 使用 16 字节随机 salt、64 MiB 内存、3 次迭代、并行度 2
  和 32 字节输出；摘要字符串保留参数，便于后续按账户渐进升级。
- 自动化 Token 的公开 `token_id` 使用 8 字节随机数编码，secret 使用 32 字节安全随机数；
  高熵 secret 只保存 SHA-256 摘要，并以常量时间比较。
- 每个管理员只有一个当前自动化 Token，可以启用、禁用或轮换。
- 文件模式为 `0600`，父目录应限制为当前用户；更新使用临时文件、同步和原子替换。
- 不热加载该文件；所有运行时变更都通过管理台完成。
- 文件不存在时创建；JSON 损坏、字段不合法或版本不支持时拒绝启动，不静默覆盖。

启动时若不存在 `admin`，Server 自动创建该管理员、随机初始密码和自动化 Token，将两份
明文只打印到启动控制台一次，并提示登录后立即改密。若管理员没有保存初始 Token，可以
登录后执行轮换，新的明文也只显示一次。

应急恢复要求停止 Server，删除 `admin` 记录或整个认证文件后重启。Server 会重新生成随机
初始凭据，不提供命令行明文密码参数。

## 6. 登录与 Session

Web 登录使用服务端内存 Session，不使用 JWT：

- Cookie 设置 `Secure`、`HttpOnly` 和 `SameSite=Strict`。
- Session ID 和 CSRF Token 分别使用至少 32 字节安全随机数。
- Session 空闲超时为 30 分钟，绝对有效期为 12 小时。
- 所有状态变更请求必须携带与 Session 绑定的 CSRF Token。
- 同一来源 IP 或同一用户名连续失败 5 次后锁定 15 分钟。
- 修改或重置密码、删除管理员时，撤销该管理员的其他全部 Session。
- 修改自己的密码后保留当前 Session，并轮换 Session ID 和 CSRF Token。
- 初始管理员或新管理员在 `must_change_password=true` 时，只能改密或退出。
- 不能删除当前登录管理员，也不能删除最后一个管理员。

所有管理员权限相同。创建管理员时生成随机初始密码和自动化 Token，明文只在创建成功页
展示一次。管理员重置其他账户密码时同样生成随机初始密码，并撤销目标账户的全部 Session；
密码重置不自动轮换自动化 Token。

## 7. 页面布局

管理台采用经典固定侧栏布局，顶部始终显示 Server 状态和当前管理员。窄屏时侧栏折叠为
菜单。页面包括：

- `/admin/login`：登录和首次改密。
- `/admin/`：在线连接、活跃会话、凭据数量、审计可用性和近期连接总览。
- `/admin/credentials`：机器凭据列表、签发和详情。
- `/admin/connections`：在线连接列表、详情和断开。
- `/admin/sessions`：用户、机器、Workspace/Tab、Agent 和状态列表。
- `/admin/audit`：受控条件的审计查询。
- `/admin/administrators`：管理员、密码重置和自动化 Token。
- `/admin/system`：Server 状态、debug 开关和停止服务。

页面由 Go 模板渲染骨架，原生 JavaScript 调用同源 JSON API 更新表格和执行操作，不引入
Node.js 构建链或前端框架。危险操作只出现在对应详情页，必须经过页面二次确认。

## 8. Web JSON API

管理 API 统一使用 `/admin/api/v1/`：

```text
auth/*                  登录、退出、改密
overview                运行总览
credentials/*           凭据签发、查看、启禁用、删除、来源地址
connections/*           连接查询、详情、断开
sessions                会话查询
audit/logs              Loki 审计查询
administrators/*        管理员、密码重置和自动化 Token
server/*                状态、debug 开关、停止
automation/credentials  外部 IT 系统签发和删除
```

成功响应：

```json
{
  "data": {},
  "request_id": "01J..."
}
```

错误响应：

```json
{
  "error": {
    "code": "credential_conflict",
    "message": "该用户和机器已存在有效凭据"
  },
  "request_id": "01J..."
}
```

状态码语义：

- `400`：请求体、字段或查询参数无效。
- `401`：未登录、Session 失效或自动化 Token 无效。
- `403`：CSRF 失败、首次改密限制或接口权限不足。
- `404`：目标不存在。
- `409`：重复签发或目标状态冲突。
- `429`：登录锁定或请求限速。
- `502`：Loki 请求、协议或结果校验失败。
- `503`：Server 正在关闭。

所有 JSON 请求使用严格解码、拒绝重复或未知字段，并设置请求体大小限制。响应包含
`request_id`，普通日志使用相同 ID 关联请求。接口不返回内部堆栈、底层路径、凭据摘要或
Loki 原始错误。

删除凭据、主动断开连接和停止 Server 等危险操作必须同时满足：

1. 页面明确显示目标并进行二次确认。
2. JSON 请求携带 `"confirm": true`。
3. Server 重新校验管理员、目标身份和当前状态。
4. 操作写入普通 HTTP 管理日志。

## 9. 外部自动化 API

外部 IT 系统使用管理员绑定的 Bearer Token，不创建浏览器 Session：

```http
POST /admin/api/v1/automation/credentials
Authorization: Bearer hpa_<token_id>_<secret>
```

请求必须包含 `principal_id`、`machine_id` 和至少一个来源地址。重复
`(principal_id, machine_id)` 明确返回 `409`，不做幂等复用。签发成功时只返回一次完整
机器 Key。

```http
DELETE /admin/api/v1/automation/credentials/{credential_id}
Authorization: Bearer hpa_<token_id>_<secret>
```

删除允许针对任意机器凭据，不限制为该 Token 签发的记录。删除语义包含禁用、持久化删除
以及立即断开现有连接。目标不存在返回 `404`。

自动化 Token 不能查询凭据、连接、会话或日志，不能启禁用凭据、修改来源、管理管理员、
切换 debug 或停止 Server。每个 Token 默认限制为每秒 5 次、滚动一分钟 100 次。限速状态
保存在内存中，Server 重启后重新计算。Token 被禁用、轮换或所属管理员被删除后立即失效。

自动化调用日志记录管理员、Token ID、动作、目标摘要、结果和耗时，不记录 Token、机器 Key
或完整用户内容。

## 10. Loki 审计查询

管理台通过 `admin.loki_url` 调用 Loki HTTP 查询接口。Server 根据受控字段构造 LogQL，
不接受任意 LogQL：

- 基础流限定为 Herdr Pal Server 审计日志。
- `userid` 使用结构化元数据精确匹配。
- `machine_id` 使用安全转义后的不区分大小写包含匹配。
- 关键字使用安全转义后的不区分大小写正文包含匹配。
- 默认查询最近 24 小时，最大范围 31 天。
- 默认每页 100 条，最大 500 条，按时间倒序。
- 使用时间戳游标翻页，不使用易漂移的页码偏移。
- 每次 Loki 请求超时 10 秒，原始响应体上限 16 MiB，并限制解析后的记录数量。

列表显示时间、用户、机器、稳定会话信息、事件类型和正文摘要，点击后展开完整用户输入或
终端文本。图片不存储和查询，只使用图片请求随附的审计文本。

Loki 超时、不可达、响应过大或协议异常只影响当前查询。错误返回稳定错误码和脱敏说明，
不泄露 Loki 原始响应。管理员查询动作写普通 HTTP 日志，但关键字只记录“是否提供”和长度，
不记录原文。

审计正文是敏感数据。页面必须显著提示管理员可以查看全部用户输入和终端输出，并避免把
展开正文长期保留在前端状态或浏览器存储中。

## 11. Web 安全边界

- TLS 最低版本为 1.2，只提供 HTTPS。
- 不启用 CORS，不信任 `X-Forwarded-For` 等代理头作为来源身份。
- 状态变更请求除 CSRF Token 外还要校验同源 `Origin`；浏览器未发送 `Origin` 时校验
  `Referer`，两者都缺失则拒绝请求。
- 设置严格 CSP，只允许同源脚本、样式和资源，禁止 iframe、外部对象和基址重写。
- 设置 `X-Content-Type-Options: nosniff` 和严格 Referrer Policy。
- HTML、CSS 和 JavaScript 由 `embed.FS` 提供，不使用第三方 CDN。
- 登录和普通 JSON 请求体上限为 64 KiB；审计查询响应遵守 16 MiB Loki 原始响应上限。
- HTTP Server 使用明确的 Header、读、写和空闲超时，不能依赖 Go 默认的无限时长。
- Handler、模板和普通日志不得输出密码、机器 Key、自动化 Token、Cookie、CSRF Token、
  审计正文或查询关键字。
- 管理操作只写普通 HTTP/Server 日志，不发送到 Loki 用户内容审计流。

## 12. HTTP 管理日志

每个请求记录：

- `request_id`
- 管理员或自动化 Token ID
- 来源地址
- HTTP method 和规范化 route
- 目标的非敏感摘要
- 结果状态和稳定错误类型
- 请求耗时

日志不得记录请求和响应完整 body。登录失败只能记录规范化用户名和来源地址；凭据签发、
管理员创建和 Token 轮换必须在明文响应生成前准备脱敏日志字段。

## 13. 测试策略

新增逻辑优先拆为可注入时钟、随机源和依赖接口的纯模块。测试至少覆盖：

- 初次启动创建管理员、认证文件权限、原子写入和随机凭据只展示一次。
- 损坏文件、未知版本、非法用户名和非法摘要导致启动失败。
- 登录、首次改密、失败锁定、空闲超时、绝对超时和 Session 撤销。
- Cookie 属性、CSRF、安全响应头、严格 JSON 和请求体限制。
- 管理员创建、重置、删除、当前账户和最后管理员保护。
- 自动化 Token 生成、摘要验证、轮换、启禁用和管理员删除后的失效。
- 自动化签发、重复签发冲突、删除任意凭据和删除后断开连接。
- Web 与 HPAP/1 对同一 `AdminService` 操作的结果一致性。
- 凭据来源地址、连接、会话、debug 和停止流程。
- Loki 查询字段转义、大小写规则、时间范围、游标分页、限制和故障降级。
- HTTP 管理日志不包含密码、Token、机器 Key、Cookie、CSRF、关键字和审计正文。
- 内嵌页面和静态资源可从最终二进制提供。

集成测试使用临时目录、临时 TLS 证书、Fake Loki 和本机随机端口，不依赖企业微信、真实
Loki 或外部网络。现有 `unittest.sh` 继续作为完整单元测试入口，`build.sh` 必须同时构建
包含管理台资源的 Server。

## 14. 验收标准

- 未安装额外 Web 资源和运行时依赖时，单个 Server 二进制可以启动管理台。
- 初始管理员可以使用控制台一次性密码登录、完成改密并管理其他管理员。
- hp-cli 的现有命令和输出兼容，Web 与 hp-cli 对同一资源执行结果一致。
- 管理员可以完成凭据、连接、会话和 Server 管理，并能按四个维度查询审计日志。
- 外部 IT 系统只能使用 Token 签发和删除机器凭据，不能访问其他管理能力。
- 任意认证文件、Loki 或管理请求错误都有明确日志和稳定响应，不泄露敏感值。
- 全部新增单元测试、集成测试和项目构建通过。
