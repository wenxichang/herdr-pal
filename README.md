# Herdr Pal

Herdr Pal 把运行在本机 Herdr 中的 Agent 接入企业微信智能机器人。你可以在企业微信单聊中
查看多台机器上的 Agent、选择会话、发送任务、读取终端输出和执行少量受限按键。

## 简单架构

```text
企业微信单聊
      │
      ▼
herdr-pal-server
      │ WSS
      ├── herdr-pal（home-mac）── 本机 Herdr
      └── herdr-pal（office-pc）─ 本机 Herdr
```

- `herdr-pal-server` 连接企业微信机器人，一套机器人只运行一个服务端。
- 每台运行 Herdr 的机器通过 Herdr `[[sidecar]]` 启动并守护一个 `herdr-pal` 客户端。
- 一个用户可以接入多台机器，并在同一个企业微信单聊中切换会话。
- Pal 与 Server 使用公开的 HPRP/1 协议；每台机器使用一把由服务端签发的独立 Key。
- `herdr-pal -i` 可以脱离企业微信，在本机终端中直接操作 Herdr Agent。

## 使用前准备

- 一台用于运行 `herdr-pal-server` 的内网机器。
- 每台客户端机器已经安装并启动 Herdr。
- Herdr 公共 Socket API protocol 必须为 `17`。
- 企业微信中有权限创建或使用智能机器人。
- 从源码构建需要 Go 1.26.5 或更高的兼容补丁版本；`go.mod` 会自动请求该工具链。

支持的平台：

- macOS AMD64/ARM64：客户端和服务端。
- Linux AMD64/ARM64：客户端和服务端，推荐用于部署服务端。
- Windows AMD64：客户端 Beta，与 Herdr Windows Beta 配套使用。

检查 Herdr：

```sh
herdr status server --json
```

输出应显示服务正在运行，且 `protocol` 为 `17`。

## 构建

```sh
./unittest.sh
./build.sh
```

构建产物：

- `dist/herdr-pal-darwin-amd64`
- `dist/herdr-pal-darwin-arm64`
- `dist/herdr-pal-linux-amd64`
- `dist/herdr-pal-linux-arm64`
- `dist/herdr-pal-server-darwin-amd64`
- `dist/herdr-pal-server-darwin-arm64`
- `dist/herdr-pal-server-linux-amd64`
- `dist/herdr-pal-server-linux-arm64`
- `dist/hp-cli-darwin-amd64`
- `dist/hp-cli-darwin-arm64`
- `dist/hp-cli-linux-amd64`
- `dist/hp-cli-linux-arm64`
- `dist/herdr-pal-windows-amd64.exe`

同时保留当前构建机器的便捷名称：

- `dist/herdr-pal-server`
- `dist/herdr-pal`
- `dist/hp-cli`

公开 GitHub Release 当前只提供 Linux 和 macOS 的 Herdr Bundle。Intel/AMD x64 选择
`amd64`，Apple Silicon 或 ARM64 Linux 选择 `arm64`。`./build.sh` 仍会生成 Pal、Server、
`hp-cli` 的分平台二进制；Windows AMD64 客户端 Beta 需要从源码构建，不发布 Windows
服务端。

为终端用户制作包含 Herdr 和 Herdr Pal 的单平台安装包：

```sh
./build.sh bundle \
  --target darwin-arm64 \
  --version "$(git describe --tags --always)" \
  --herdr-source ~/Code/herdr
```

`--target` 支持 `darwin-amd64`、`darwin-arm64`、`linux-amd64` 和 `linux-arm64`。跨平台构建
Herdr 不方便时，也可以把现有 Herdr 构建产物通过 `--herdr-binary` 传入。生成结果为
`dist/herdr-bundle-<版本>-<目标>.tar.gz` 和同名 `.sha256`。

## 第一步：申请企业微信智能机器人

企业微信不同版本和用户权限下的入口可能略有差异，一般按以下步骤操作：

1. 管理员登录企业微信管理后台，进入“安全与管理” → “管理工具” → “智能机器人”；普通
   成员也可以在企业微信客户端“工作台”中查找“智能机器人”。
2. 创建机器人，选择“手动创建”或“API 模式创建”。
3. 在 API 配置中选择“使用长连接”。
4. 设置机器人名称、头像和可见范围。需要使用 Herdr Pal 的成员必须处于可见范围内。
5. 保存机器人，并记录 `Bot ID` 和 `Secret`。

`Secret` 直接写入权限为 `0600` 的服务端配置文件，不要提交到 Git。企业微信界面发生变化
时，可参考[企业微信智能机器人官方文档](https://developer.work.weixin.qq.com/document/path/101463)。

## 第二步：启动服务端

创建默认配置目录并复制示例：

```sh
mkdir -p ~/.config/herdr-pal-server
chmod 700 ~/.config/herdr-pal-server
cp server-config.example.json ~/.config/herdr-pal-server/server.json
```

编辑 `~/.config/herdr-pal-server/server.json`：

```json
{
  "wecom": {
    "bot_id": "你的 Bot ID",
    "secret": "你的机器人 Secret"
  },
  "server": {
    "listen": "0.0.0.0:9443",
    "cert_file": "",
    "key_file": "",
    "state_dir": "",
    "credentials_file": ""
  },
  "admin": {
    "listen": "0.0.0.0:4001",
    "loki_url": ""
  },
  "rate_limit": {
    "per_second": 1,
    "per_minute": 20
  },
  "audit": {
    "type": "none",
    "endpoint": "",
    "skip_verify": false,
    "stderr": false
  },
  "log": {
    "level": "info"
  }
}
```

企业微信 `/help` 的完整内容保存在 `~/.config/herdr-pal-server/help.md`。首次启动会生成默认
文件；此后每次 `/help` 都重新读取磁盘，不缓存内容，Server 也不会覆盖已经存在的文件。默认
帮助会引导用户先用 `/reg` 注册机器，再使用其中的 WSS 地址、机器 Key 和安装步骤；管理员可
直接修改该文件，保存后无需重启 Server 即可生效。

`rate_limit` 按企业微信用户限制唯一输入，默认每秒 1 条、滚动 60 秒内 20 条。字段缺省使用
默认值，显式设置为 `0` 会关闭对应窗口；重复投递的同一企业微信消息不会重复计数。

`audit.type` 只支持 `none` 或 `otlp`。使用 OTLP/HTTP protobuf Logs 时填写完整目标地址，
Server 会原样使用其中的路径。可以直连 Loki 原生 OTLP 接口：

```json
"audit": {
  "type": "otlp",
  "endpoint": "http://127.0.0.1:3100/otlp/v1/logs",
  "skip_verify": false,
  "stderr": false
}
```

使用 OpenTelemetry Collector 时通常配置为
`https://otel-collector.internal:4318/v1/logs`。endpoint 必须是绝对 HTTP(S) URL，且不能
包含 userinfo、query 或 fragment。

需要认证 Header 时使用 OpenTelemetry 标准环境变量，值按 URL 编码：

```sh
export OTEL_EXPORTER_OTLP_LOGS_HEADERS='Authorization=Bearer%20collector-token,x-tenant=team-a'
```

审计记录用户输入和终端文本输出；图片模式只记录图片配套文本，不记录 PNG。OTLP 不可用、
队列满或关闭刷新超时都不会阻断用户操作。`audit.stderr=true` 会额外向 stderr 输出包含完整
审计正文的 JSON Lines，只应在受控调试环境启用。

配置文件包含企业微信 Secret，请限制为当前用户可读，然后启动：

```sh
chmod 600 ~/.config/herdr-pal-server/server.json
./dist/herdr-pal-server
```

首次启动时，Server 会在标准输出中显示一次默认管理员 `admin` 的随机初始密码和自动化
Token，并同步写入权限为 `0600` 的 `~/.config/herdr-pal-server/bootstrap.txt`。请立即登录
修改密码并妥善保存 Token；认证文件已存在时不会重新生成或覆盖引导文件。浏览器访问：

```text
https://服务端地址:4001/admin/
```

证书路径留空时，管理台与 Relay 共用 Server 自动生成的自签名证书，浏览器会出现证书告警；
确认指纹和服务端身份后再继续。首次登录只能修改初始密码，改密后即可：

- 签发、启用、禁用和删除机器 Key，维护来源地址规则。
- 审批或驳回用户从企业微信提交的后续机器注册申请。
- 查看 HPRP 在线连接与各用户、各机器上的 Agent 会话。
- 修改自己的密码；创建其他管理员、重置他人密码并轮换或禁用自动化 Token。
- 切换详细日志、查看运行状态和优雅停止 Server。
- 配置 `admin.loki_url` 后按用户、机器、日期范围和关键字查询审计日志。
- 在“系统”页面查看外部 IT 系统签发和删除机器 Key 的接入指南。

管理员认证文件固定为 `~/.config/herdr-pal-server/auth.json`，只保存 Argon2id 密码摘要和
自动化 Token 摘要，Unix 权限必须保持 `0600`。文件损坏、版本无效或权限过宽时 Server 会
拒绝启动并明确报告路径；不要手工编辑其内容。忘记密码时应使用另一个管理员重置，或在
明确接受重建全部管理员身份的情况下先备份再移走该文件并重启。

`admin.listen` 默认是 `0.0.0.0:4001`，只提供 HTTPS。`admin.loki_url` 是 Loki 基础地址，
例如 `http://127.0.0.1:3100`；留空时只有审计查询页不可用，不影响企业微信、HPRP、HPAP
和 OTLP 审计写入。

需要排查连接、会话上报或企微消息转发问题时，使用详细日志模式：

```sh
./dist/herdr-pal-server --verbose
```

`--verbose` 会把服务端日志级别临时提升为 `debug`，记录 Relay 握手阶段、客户端版本、快照
序号和会话数量、心跳、企微交互动作、路由目标、消息分段及错误码。日志只记录消息长度、
credential ID，以及用户/message/session 的摘要，不记录 prompt、终端快照正文、完整用户 ID
或机器 Key；当前 Bot Secret 和 OTLP Header 值即使出现在底层错误中也会替换为
`[REDACTED]`。启用的业务审计流属于独立敏感输出，不受这条普通运行日志规则影响。

未指定 `-config` 时，服务端默认读取：

```text
~/.config/herdr-pal-server/server.json
```

需要使用其他文件时：

```sh
./dist/herdr-pal-server -config /绝对路径/server.json
```

证书路径留空时，服务端会自动生成自签名证书。首版面向受信任内网使用，不要直接暴露到
互联网。

`credentials_file` 留空时默认使用 `state_dir/credentials.json`；服务端只保存 Key 摘要，
不会保存可直接连接的明文 Key。

待审批机器申请固定保存在 `state_dir/registrations.json`，只包含尚未处理的 pending。批准或
驳回后记录立即删除，历史只进入 `herdr_pal.machine_registration` OTLP/Loki 业务审计事件。

服务端同时在 `<state_dir>/admin.sock` 启动仅限同一系统用户访问的本地管理接口。确认服务
已经可以管理：

```sh
./dist/hp-cli server status
```

`hp-cli` 默认读取同一个 `~/.config/herdr-pal-server/server.json` 来定位 Admin Socket，但不
使用其中的企业微信 Secret。服务端使用其他配置文件时，给 `hp-cli` 传入相同的 `-config`。

查看管理命令和参数帮助：

```sh
./dist/hp-cli --help
./dist/hp-cli help key
./dist/hp-cli key issue --help
```

顶层帮助会列出全部命令路径；子命令帮助会列出下一级命令，叶子命令帮助会说明位置参数、
可选参数、格式约束和示例。帮助命令不读取配置，也不要求服务端正在运行。

### 自动化签发与删除

每个管理员都有独立的 `hpa_...` 自动化 Token。Token 仅允许签发和删除机器凭据，不能查询
运行数据或修改管理员。把 Token 放入受控环境变量，不要写入脚本或命令历史：

```sh
export HERDR_PAL_ADMIN_TOKEN='管理员界面中一次性显示的 hpa_ Token'

curl -k --fail-with-body \
  -H "Authorization: Bearer ${HERDR_PAL_ADMIN_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{"principal_id":"企业微信用户 ID","machine_id":"office-pc","sources":["192.168.1.20"]}' \
  https://服务端地址:4001/admin/api/v1/automation/credentials
```

签发响应中的 `hpk_...` 机器 Key 同样只返回一次。删除凭据会立即使 Key 失效并断开对应 Pal：

```sh
curl -k --fail-with-body -X DELETE \
  -H "Authorization: Bearer ${HERDR_PAL_ADMIN_TOKEN}" \
  https://服务端地址:4001/admin/api/v1/automation/credentials/凭据ID
```

自动化接口按 Token 限制为每秒 5 次、滚动一分钟 100 次。浏览器管理操作继续使用管理员
Session、同源校验和 CSRF 防护，不能用自动化 Token 代替浏览器登录。

## 第三步：注册机器并获取 Key

终端用户优先在企业微信机器人单聊中自主注册：

```text
/reg office-pc 192.168.1.20
```

第一个参数是当前运行 Herdr 的机器标识，第二个参数是 Pal 连接 Server 时的真实来源地址。
多个来源使用逗号分隔，例如：

```text
/reg office-pc 192.168.1.20,10.0.0.0/24
```

如果该企业微信用户没有任何机器凭据或待审批申请，Server 会直接签发首台机器 Key，并在
当前单聊响应中只显示一次。如果用户已有机器，申请会进入 Web 管理台“注册审批”页面；批准
后 Server 主动把 Key 发给申请人。审批发送失败时，新凭据会被回滚，申请继续保留以便重试。
收到 Key 后发送 `/help`，按管理员维护的安装地址和步骤继续部署。

管理员人工签发仍作为备用路径。管理员从企业微信管理信息或组织内账户系统取得用户的企业
微信 principal ID，并为每台接入机器确定一个在该用户下不重复的机器标识，例如
`office-pc`。这些身份信息只在服务端签发凭据时使用，终端用户无需写入 Herdr Pal 配置。
管理员在服务端执行：

```sh
./dist/hp-cli key issue \
  --principal-id '企业微信返回的用户 ID' \
  --machine-id 'office-pc' \
  --source '192.168.1.20'
```

命令只在签发时输出一次 `hpk_...` Key。请通过安全渠道交给对应机器，不要写入聊天记录、
日志或 Git。`--source` 必须至少提供一次，可重复使用；支持单 IP、CIDR
（`192.168.1.0/24`）和闭区间（`192.168.1.20-192.168.1.30`）。这里填写 Pal 实际连接
Server 时的来源地址，不信任 `X-Forwarded-For` 等代理头。不同用户可以使用相同机器标识；
同一用户的每台机器必须使用独立 Key。

## 第四步：启动每台客户端

Linux 和 macOS 推荐直接使用同时包含 Herdr、Herdr Pal 和安装器的一体化包。从 Release
下载与本机匹配的文件：

- Apple Silicon：`herdr-bundle-<版本>-darwin-arm64.tar.gz`
- Intel Mac：`herdr-bundle-<版本>-darwin-amd64.tar.gz`
- Linux x64：`herdr-bundle-<版本>-linux-amd64.tar.gz`
- Linux ARM64：`herdr-bundle-<版本>-linux-arm64.tar.gz`

使用同名 `.sha256` 校验文件后解压，在包目录执行：

```sh
./install.sh
```

安装器会交互完成以下操作：

- 选择安装目录，默认 `~/.local/bin`，也可以输入其他用户可写目录。
- 输入服务端 `wss://` URL 和管理员为本机签发的 Key；Key 会在终端回显，请确认粘贴完整并
  避免旁人看到。
- 把 `herdr` 和 `herdr-pal` 安装为同目录的真实文件，旧文件先生成带时间戳的备份。
- 合并并备份 `~/.config/herdr-pal/config.json` 和 `~/.config/herdr/config.toml`。
- 添加幂等的 `[[sidecar]] command = ["herdr-pal"]`，让 Herdr 在自身生命周期内启动、守护并
  停止 Pal。
- 检测到 Herdr 已运行时，询问是否执行 `live-handoff`，默认执行且不会清空现有 pane。

安装完成后不需要手工启动 `herdr-pal`。启动 Herdr，或让安装器完成 `live-handoff`，再回到
企业微信执行 `/ls`。

高级用户需要手工部署或排错时，Pal 配置仍使用：

```json
{
  "relay": {
    "url": "wss://服务端地址:9443",
    "key": "企微注册或管理员签发的 hpk_ 机器 Key",
    "skip_verify": true
  },
  "herdr": {
    "session": "",
    "socket_path": ""
  },
  "log": {
    "level": "info"
  }
}
```

字段说明：

- `url` 必须使用 `wss://`，地址指向 `herdr-pal-server`。
- `key` 填写企微注册或管理员为当前机器签发的完整 `hpk_...` Key。用户和机器身份都由该 Key 绑定，
  客户端不能自行声明或覆盖。
- 使用服务端自动生成的自签名证书时保持 `skip_verify: true`。
- 默认 Herdr session 下，`session` 和 `socket_path` 都留空。
- 使用命名 Herdr session 时填写 `session`；只有自动探测失败时才手工填写
  `socket_path`。

手工启动客户端：

```sh
./dist/herdr-pal
```

Windows PowerShell 使用 AMD64 Beta 版本：

```powershell
New-Item -ItemType Directory -Force "$env:USERPROFILE\.config\herdr-pal" | Out-Null
Copy-Item .\config.example.json "$env:USERPROFILE\.config\herdr-pal\config.json"
.\dist\herdr-pal-windows-amd64.exe
```

Windows 默认配置文件是 `%USERPROFILE%\.config\herdr-pal\config.json`。Herdr 的
`session` 和 `socket_path` 通常保持为空，客户端会调用 Herdr CLI 获取 marker 路径，并
通过对应的 Windows Named Pipe 连接。Windows 版本目前随 Herdr Windows 一同按 Beta
支持，建议先在非关键会话中验证 Agent、终端和输入法行为。

未指定 `-config` 时，客户端默认读取：

```text
~/.config/herdr-pal/config.json
```

需要使用其他文件时：

```sh
./dist/herdr-pal -config /绝对路径/client.json
```

同一用户、同一机器标识同时只能有一个客户端连接；重复连接会收到 HTTP `409` 并被拒绝。

客户端上线后，管理员可以在服务端查看连接和用户看到的 Agent 会话：

```sh
./dist/hp-cli connection list
./dist/hp-cli session list
```

机器停用时使用 `hp-cli key disable <ID>`：它会持久化禁用 Key，并立即移除对应连接和会话，
Pal 后续也不能重连。临时排查连接时使用 `hp-cli connection disconnect <CONNECTION_ID>`：它
只断开当前连接，不改变 Key，Pal 可以自动重连。恢复已禁用的 Key 使用
`hp-cli key enable <ID>`；不可恢复删除必须显式执行 `hp-cli key delete <ID> --yes`。

常用的来源策略维护命令：

```sh
./dist/hp-cli key source list 1
./dist/hp-cli key source add 1 192.168.1.0/24
./dist/hp-cli key source set 1 192.168.1.20 10.0.0.1-10.0.0.5
```

所有管理查询支持 `--json`，便于脚本审计和监控。完整命令与安全边界见
[HPAP 本地管理面](docs/HPAP_ADMIN_DESIGN.md)。

## 第五步：开始使用

回到企业微信机器人单聊：

1. 发送 `/ls` 查看所有在线机器上的 Agent。
2. 发送 `/1` 选择第一个会话。选择成功后会立即返回该会话最近的终端内容。
3. 直接输入普通文本，把任务发送给当前 Agent。
4. Agent 完成任务、阻塞或需要关注时，客户端只上报状态变化，由服务端按当前会话显示模式
   拉取并发送最近的终端输出。

也可以使用 `/2 继续处理` 直接在第 2 个会话执行，并在成功后切换到它；使用
`#2 /con` 可以临时查看第 2 个会话而不改变当前选择。

多台机器接入时，`/ls` 会统一编号。编号可能随机器或会话变化，重新执行 `/ls` 后应按
最新列表选择。

## 企业微信命令

| 输入 | 作用 |
| --- | --- |
| `/ls` | 查看当前用户的全部在线 Agent |
| `/reg office-pc 192.168.1.20` | 注册机器；多个来源用逗号分隔 |
| `/1` | 选择列表中的第 1 个会话并显示最近输出 |
| `/1 内容` | 在第 1 个会话执行内容，成功后切换到该会话 |
| `#1 内容` | 在第 1 个会话执行内容，但保持当前选择 |
| `/help` | 显示命令帮助 |
| `/con` | 显示当前 Agent 最近 100 行并重置分页 |
| `/pageup`、`/pagedn` | 上翻或下翻终端缓存 |
| `/mode img` | 当前会话使用终端图片，保留 ANSI 颜色和选中样式 |
| `/mode txt` | 当前会话使用纯文本 |
| `/enter`、`/key enter` | 发送 Enter |
| `/key up`、`/key down` | 发送方向键 |
| `/key space`、`/key esc` | 发送空格或 Esc |
| `/key down,space,down` | 连续发送多个按键 |
| `/slash clear` | 向 Agent 发送 `/clear` |
| 普通文本 | 发送给当前选择的 Agent |

定向前缀后的内容可以是普通文本或 `/con`、`/key`、`/enter`、`/slash` 等客户端命令，
不能是 `/ls`、`/help`、`/reg` 或另一个 `/N`。`/N 内容` 只有在目标客户端
成功响应后才会更新当前选择；失败或超时时保留原选择并明确提示。

`/ls` 中的状态会显示为 `done ✅`、`working ⏳`、`blocked ⁉️`、`idle 💤` 或
`unknown ❔`，企业微信和本机交互模式保持一致。

`/key` 支持 `up`、`down`、`esc`、`space`、单个 ASCII 字母和数字；`dn` 可代替
`down`，`sp` 可代替 `space`。多个按键可用逗号或空格分隔，间隔 100ms。
全部按键发送完成后会等待 200ms，再自动读取一次 `/con` 内容；`enter` 只能单独发送。

`/con` 和翻页命令以 100 行为一页。通知也只读取最近 100 行，较早内容可以通过
`/pageup` 查看。

终端显示模式只作用于当前稳定会话，并由服务端保存在内存中，服务端重启后恢复默认值。
OpenCode 会话默认使用图片模式，其他 Agent 默认使用文本模式；可以随时使用 `/mode img`
或 `/mode txt` 切换。图片由对应机器上的 Pal 使用内嵌等宽字体渲染为 16px、最多 256 色的
PNG，同时把同一次读取的纯文本传给服务端，用于发送失败时降级以及后续审计。图片上传或
渲染失败时会自动退回文本，本次会话的模式设置不会被改变；服务端不持久保存终端图片。

企业微信中的终端页会在开头和末尾显示 `[终端输出#N]`，其中 `N` 是当前全局 `/ls`
列表序号。尚未执行 `/ls` 时，服务端会按当前在线会话自动建立编号。输出来源不是当前选择
时，消息末尾会提示使用对应的 `/N` 切换到该输出会话。

同一用户最近 2 分钟内有过输入或收到终端事件时，其他会话的新终端通知只发送一条简短
提醒，避免大段后台输出打断当前交互；当前会话回复和命令后续分段仍发送完整内容。任一方向
的新交互都会重新计算这 2 分钟。

普通文本只会发送给当前处于可输入状态的 Agent；Agent 正在工作或等待人工操作时，会返回
提示而不会强行插入新任务。

如果发送成功后 Agent 在同一面板内创建了新会话，客户端和服务端会同步更新当前选择；后续
`/ls`、普通输入和完成通知继续指向新会话，不需要重新执行 `/N`。

## 本机交互模式

不连接企业微信和服务端，直接在当前终端中操作 Herdr：

```sh
./dist/herdr-pal -i
```

交互模式默认自动探测 Herdr，不读取 `~/.config/herdr-pal/config.json`。需要指定 Herdr
session、Socket 或日志级别时，显式传入仅包含 `herdr` 和 `log` 的配置：

```sh
./dist/herdr-pal -i -config /绝对路径/interactive.json
```

## 日志与常见问题

服务端和客户端默认把日志写到 stderr，不会自动创建 `herdr-pal.log`。需要保存日志时：

```sh
./dist/herdr-pal-server --verbose 2>&1 | tee herdr-pal-server.log
./dist/herdr-pal 2>&1 | tee herdr-pal.log
```

服务端默认记录启动、连接和异常等运行信息；`--verbose` 额外记录客户端快照上报、心跳、
企业微信入站交互和出站发送结果。每条错误日志都包含 `error_type` 和 `reason`，企业微信
协议错误还会包含 `error_code`。

客户端默认记录 Herdr 协议版本、会话数量、Relay 连接阶段、断开原因和重试间隔。
需要查看快照上报、心跳、选择和命令执行细节时，把客户端 `config.json` 中的日志级别改为：

```json
"log": {
  "level": "debug"
}
```

客户端日志会标明 `component`、`stage`、`credential_id`、`machine_id`、`pane_id`、连接地址、
快照序号、会话数、请求哈希及具体错误原因。不记录完整 Key、原始用户 ID、URL 查询参数、
消息正文或终端内容。

### 提示配置错误

- 检查默认配置文件是否存在且 JSON 格式正确。
- 服务端确认 `wecom.bot_id` 和 `wecom.secret` 已填写。
- 客户端确认 `url` 和管理员签发的 `key` 已填写。

服务端会同时打印配置文件路径和具体原因，例如：

```text
配置错误（/home/user/.config/herdr-pal-server/server.json）：缺少必填字段 wecom.secret
```

客户端和本机交互模式也会打印对应配置路径及加载或校验原因，例如：

```text
配置错误（/home/user/.config/herdr-pal/config.json）：读取配置文件: open ...: no such file or directory
```

其他服务端启动或运行错误也会直接打印完整原因；错误内容中的当前机器人 Secret 会替换为
`[REDACTED]`。

### 服务端无法连接企业微信

- 确认机器人选择的是 API 长连接模式。
- 检查 Bot ID 和 Secret 是否属于同一个机器人。
- 检查服务端能否访问企业微信网络。

### `/ls` 没有会话

- 确认目标机器上的 Herdr 正在运行。
- 检查 `herdr status server --json` 是否显示 protocol 17。
- 查看客户端日志是否已经连接 Relay，并成功上报会话。
- 确认管理员签发 Key 时绑定的 principal ID 与企业微信回调中的用户身份完全一致。
- 确认客户端使用的是为当前机器签发的完整 Key，且服务端凭据文件没有被覆盖。
- 新建或启动 Agent 后，客户端最长约 10 秒会主动刷新 Herdr 会话并上报；随后重新执行
  `/ls`。

### 客户端无法连接服务端

- 确认 `relay.url` 使用 `wss://`，地址和端口可从客户端机器访问。
- 使用自动证书时确认 `skip_verify` 为 `true`。
- 若日志显示 HTTP `409`，检查同一用户是否已有相同机器标识的客户端在线。
- 若日志显示 HTTP `401`，Key 无效、已过期、已禁用、来源地址不符合规则，或服务端读取了
  错误的凭据文件。可在服务端执行 `hp-cli key show <ID>` 和 `hp-cli key source list <ID>`。

### Herdr Socket 自动探测失败

作为 Herdr Sidecar 运行时，客户端先使用 Herdr 注入的 `HERDR_SOCKET_PATH`。非 Sidecar
部署会调用 Herdr 公共 CLI；默认 session 查询失败时，还会尝试：

```text
$HOME/.config/herdr/herdr.sock
```

Windows 会依次按 `XDG_CONFIG_HOME`、`APPDATA`、`USERPROFILE`、`HOME` 推导 Herdr 默认
marker 路径，并尝试连接对应的 Named Pipe。

命名 session 不猜测路径。如果仍然失败，在客户端配置中显式填写 `herdr.socket_path`。

## 安全提示

- 只在受信任内网部署当前版本。
- 不要把 Bot Secret、Cookie 或用户凭据提交到仓库。
- 每台机器使用独立 Key；机器停用时使用 `hp-cli key disable` 或 `key delete`。
- Herdr Pal 不会自动批准权限请求。
- Relay 断线期间不会缓存或补发旧任务。

## 开发与技术文档

查看版本：

```sh
./dist/herdr-pal-server --version
./dist/herdr-pal --version
./dist/hp-cli --version
```

运行完整检查：

```sh
./unittest.sh
```

更详细的架构、Herdr API 审计和维护上下文：

- [Bridge 架构](docs/BRIDGE_ARCHITECTURE.md)
- [Herdr API 审计](docs/HERDR_API_AUDIT.md)
- [维护交接](docs/HANDOFF_CONTEXT.md)
- [HPRP/1 协议设计](docs/HPRP_PROTOCOL_DESIGN.md)
- [HPAP/1 本地管理面](docs/HPAP_ADMIN_DESIGN.md)
- [Windows AMD64 支持](docs/WINDOWS_AMD64_SUPPORT.md)
