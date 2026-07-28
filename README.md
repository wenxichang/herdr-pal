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
- 每台运行 Herdr 的机器启动一个 `herdr-pal` 客户端。
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

GitHub Release 只发布带操作系统和架构后缀的文件。Intel/AMD x64 选择 `amd64`，Apple
Silicon 或 ARM64 Linux 选择 `arm64`。Windows 当前只发布客户端 AMD64 Beta，不发布
Windows 服务端。

## 第一步：申请企业微信智能机器人

企业微信不同版本和用户权限下的入口可能略有差异，一般按以下步骤操作：

1. 管理员登录企业微信管理后台，进入“安全与管理” → “管理工具” → “智能机器人”；普通
   成员也可以在企业微信客户端“工作台”中查找“智能机器人”。
2. 创建机器人，选择“手动创建”或“API 模式创建”。
3. 在 API 配置中选择“使用长连接”。
4. 设置机器人名称、头像和可见范围。需要使用 Herdr Pal 的成员必须处于可见范围内。
5. 保存机器人，并记录 `Bot ID` 和 `Secret`。

`Secret` 只通过环境变量交给服务端，不要写入配置文件或提交到 Git。企业微信界面发生变化
时，可参考[企业微信智能机器人官方文档](https://developer.work.weixin.qq.com/document/path/101463)。

## 第二步：启动服务端

创建默认配置目录并复制示例：

```sh
mkdir -p ~/.config/herdr-pal
cp server-config.example.json ~/.config/herdr-pal/server-config.json
```

编辑 `~/.config/herdr-pal/server-config.json`：

```json
{
  "wecom": {
    "bot_id": "你的 Bot ID"
  },
  "server": {
    "listen": "0.0.0.0:9443",
    "addr_hint": "10.1.3.4",
    "cert_file": "",
    "key_file": "",
    "state_dir": "",
    "credentials_file": ""
  },
  "log": {
    "level": "info"
  }
}
```

`addr_hint` 填写客户端机器能够访问的服务端主机名或 IP，不要包含 `wss://`、端口或路径。
服务端会自动使用 `listen` 中的端口，在企业微信 `/help` 的客户端配置示例中生成
`wss://10.1.3.4:9443`。留空时 `/help` 继续显示需要向管理员获取地址的占位提示。

设置 Secret 并启动：

```sh
export HERDR_PAL_WECOM_SECRET='你的机器人 Secret'
./dist/herdr-pal-server
```

需要排查连接、会话上报或企微消息转发问题时，使用详细日志模式：

```sh
./dist/herdr-pal-server --verbose
```

`--verbose` 会把服务端日志级别临时提升为 `debug`，记录 Relay 握手阶段、客户端版本、快照
序号和会话数量、心跳、企微交互动作、路由目标、消息分段及错误码。日志只记录消息长度、
credential ID，以及用户/message/session 的摘要，不记录 prompt、终端快照正文、完整用户 ID
或机器 Key；当前 Bot Secret 即使出现在底层错误中也会替换为 `[REDACTED]`。

未指定 `-config` 时，服务端默认读取：

```text
~/.config/herdr-pal/server-config.json
```

需要使用其他文件时：

```sh
./dist/herdr-pal-server -config /绝对路径/server.json
```

证书路径留空时，服务端会自动生成自签名证书。首版面向受信任内网使用，不要直接暴露到
互联网。

`credentials_file` 留空时默认使用 `state_dir/credentials.json`；服务端只保存 Key 摘要，
不会保存可直接连接的明文 Key。

服务端同时在 `<state_dir>/admin.sock` 启动仅限同一系统用户访问的本地管理接口。确认服务
已经可以管理：

```sh
./dist/hp-cli server status
```

`hp-cli` 默认读取同一个 `~/.config/herdr-pal/server-config.json` 来定位 Admin Socket，不
需要企业微信 Secret。服务端使用其他配置文件时，给 `hp-cli` 传入相同的 `-config`。

## 第三步：获取用户 ID 并签发机器 Key

服务端成功连接企业微信后，在自己的机器人单聊中发送：

```text
/userid
```

机器人会返回当前企业微信用户 ID。把它交给服务端管理员，并为每台接入机器确定一个易识别
且在该用户下不重复的机器标识，例如 `office-pc`。管理员在服务端执行：

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

在每台运行 Herdr 的机器上执行：

```sh
mkdir -p ~/.config/herdr-pal
cp config.example.json ~/.config/herdr-pal/config.json
```

编辑 `~/.config/herdr-pal/config.json`：

```json
{
  "relay": {
    "url": "wss://服务端地址:9443",
    "key": "管理员签发的 hpk_ 机器 Key",
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

配置说明：

- `url` 必须使用 `wss://`，地址指向 `herdr-pal-server`。
- `key` 填写管理员为当前机器签发的完整 `hpk_...` Key。用户和机器身份都由该 Key 绑定，
  客户端不能自行声明或覆盖。
- 使用服务端自动生成的自签名证书时保持 `skip_verify: true`。
- 默认 Herdr session 下，`session` 和 `socket_path` 都留空。
- 使用命名 Herdr session 时填写 `session`；只有自动探测失败时才手工填写
  `socket_path`。

启动客户端：

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
4. Agent 完成任务、阻塞或需要关注时，机器人会主动发送最近的终端输出。

也可以使用 `/2 继续处理` 直接在第 2 个会话执行，并在成功后切换到它；使用
`#2 /con` 可以临时查看第 2 个会话而不改变当前选择。

多台机器接入时，`/ls` 会统一编号。编号可能随机器或会话变化，重新执行 `/ls` 后应按
最新列表选择。

## 企业微信命令

| 输入 | 作用 |
| --- | --- |
| `/userid` | 查看自己的企业微信用户 ID，供管理员签发机器 Key |
| `/ls` | 查看当前用户的全部在线 Agent |
| `/1`、`/sel 1` | 选择列表中的第 1 个会话并显示最近输出 |
| `/1 内容` | 在第 1 个会话执行内容，成功后切换到该会话 |
| `#1 内容` | 在第 1 个会话执行内容，但保持当前选择 |
| `/help` | 显示命令帮助 |
| `/con` | 显示当前 Agent 最近 100 行并重置分页 |
| `/pageup`、`/pagedn` | 上翻或下翻终端缓存 |
| `/enter`、`/key enter` | 发送 Enter |
| `/key up`、`/key down` | 发送方向键 |
| `/key space`、`/key esc` | 发送空格或 Esc |
| `/key down,space,down` | 连续发送多个按键 |
| `/slash clear` | 向 Agent 发送 `/clear` |
| 普通文本 | 发送给当前选择的 Agent |

定向前缀后的内容可以是普通文本或 `/con`、`/key`、`/enter`、`/slash` 等客户端命令，
不能是 `/userid`、`/ls`、`/help`、另一个 `/N` 或 `/sel N`。`/N 内容` 只有在目标客户端
成功响应后才会更新当前选择；失败或超时时保留原选择并明确提示。

`/ls` 中的状态会显示为 `done ✅`、`working ⏳`、`blocked ⁉️`、`idle 💤` 或
`unknown ❔`，企业微信和本机交互模式保持一致。

`/key` 支持 `up`、`down`、`esc`、`space`、单个 ASCII 字母和数字；`dn` 可代替
`down`，`sp` 可代替 `space`。多个按键可用逗号或空格分隔，间隔 100ms。
全部按键发送完成后会等待 200ms，再自动读取一次 `/con` 内容；`enter` 只能单独发送。

`/con` 和翻页命令以 100 行为一页。通知也只读取最近 100 行，较早内容可以通过
`/pageup` 查看。

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
- 服务端确认已设置 `HERDR_PAL_WECOM_SECRET`。
- 客户端确认 `url` 和管理员签发的 `key` 已填写。

服务端会同时打印配置文件路径和具体原因，例如：

```text
配置错误（/home/user/.config/herdr-pal/server-config.json）：缺少环境变量 HERDR_PAL_WECOM_SECRET
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
- 确认管理员签发 Key 时使用的用户 ID 与机器人 `/userid` 返回值完全一致。
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

客户端先调用 Herdr 公共 CLI。默认 session 查询失败时，还会尝试：

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
