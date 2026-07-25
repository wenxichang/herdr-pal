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
- `herdr-pal -i` 可以脱离企业微信，在本机终端中直接操作 Herdr Agent。

## 使用前准备

- 一台用于运行 `herdr-pal-server` 的内网机器。
- 每台客户端机器已经安装并启动 Herdr。
- Herdr 公共 Socket API protocol 必须为 `17`。
- 企业微信中有权限创建或使用智能机器人。
- 从源码构建需要 Go 1.26。

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

- `dist/herdr-pal-server`
- `dist/herdr-pal`

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
    "cert_file": "",
    "key_file": "",
    "state_dir": ""
  },
  "log": {
    "level": "info"
  }
}
```

设置 Secret 并启动：

```sh
export HERDR_PAL_WECOM_SECRET='你的机器人 Secret'
./dist/herdr-pal-server
```

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

## 第三步：获取企业微信 userid

服务端成功连接企业微信后，在自己的机器人单聊中发送：

```text
/userid
```

机器人会返回当前企业微信用户的完整 `userid`。复制该值，下一步填写到客户端配置中。

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
    "userid": "通过 /userid 获取的值",
    "machine_id": "home-mac",
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
- `userid` 原样填写机器人返回的值。
- `machine_id` 是企业微信中显示的机器名称，同一用户的每台机器应使用不同值。
- 使用服务端自动生成的自签名证书时保持 `skip_verify: true`。
- 默认 Herdr session 下，`session` 和 `socket_path` 都留空。
- 使用命名 Herdr session 时填写 `session`；只有自动探测失败时才手工填写
  `socket_path`。

启动客户端：

```sh
./dist/herdr-pal
```

未指定 `-config` 时，客户端默认读取：

```text
~/.config/herdr-pal/config.json
```

需要使用其他文件时：

```sh
./dist/herdr-pal -config /绝对路径/client.json
```

同一 `userid + machine_id` 同时只能有一个客户端连接；重复连接会被拒绝。

## 第五步：开始使用

回到企业微信机器人单聊：

1. 发送 `/ls` 查看所有在线机器上的 Agent。
2. 发送 `/1` 选择第一个会话。选择成功后会立即返回该会话最近的终端内容。
3. 直接输入普通文本，把任务发送给当前 Agent。
4. Agent 完成任务、阻塞或需要关注时，机器人会主动发送最近的终端输出。

多台机器接入时，`/ls` 会统一编号。编号可能随机器或会话变化，重新执行 `/ls` 后应按
最新列表选择。

## 企业微信命令

| 输入 | 作用 |
| --- | --- |
| `/userid` | 查看自己的企业微信 userid |
| `/ls` | 查看当前用户的全部在线 Agent |
| `/1`、`/sel 1` | 选择列表中的第 1 个会话并显示最近输出 |
| `/help` | 显示命令帮助 |
| `/con` | 显示当前 Agent 最近 100 行并重置分页 |
| `/pageup`、`/pagedn` | 上翻或下翻终端缓存 |
| `/enter`、`/key enter` | 发送 Enter |
| `/key up`、`/key down` | 发送方向键 |
| `/key space`、`/key esc` | 发送空格或 Esc |
| `/key down,space,down` | 连续发送多个按键 |
| `/slash clear` | 向 Agent 发送 `/clear` |
| 普通文本 | 发送给当前选择的 Agent |

`/key` 支持 `up`、`down`、`esc`、`space`、单个 ASCII 字母和数字；`dn` 可代替
`down`，`sp` 可代替 `space`。多个按键可用逗号或空格分隔，间隔 100ms。
`enter` 只能单独发送。

`/con` 和翻页命令以 100 行为一页。通知也只读取最近 100 行，较早内容可以通过
`/pageup` 查看。

普通文本只会发送给当前处于可输入状态的 Agent；Agent 正在工作或等待人工操作时，会返回
提示而不会强行插入新任务。

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
./dist/herdr-pal-server 2>&1 | tee herdr-pal-server.log
./dist/herdr-pal 2>&1 | tee herdr-pal.log
```

### 提示配置错误

- 检查默认配置文件是否存在且 JSON 格式正确。
- 服务端确认已设置 `HERDR_PAL_WECOM_SECRET`。
- 客户端确认 `url`、`userid` 和 `machine_id` 已填写。

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
- 确认客户端配置的 `userid` 与机器人 `/userid` 返回值完全一致。

### 客户端无法连接服务端

- 确认 `relay.url` 使用 `wss://`，地址和端口可从客户端机器访问。
- 使用自动证书时确认 `skip_verify` 为 `true`。
- 检查同一用户是否已经有相同 `machine_id` 的客户端在线。

### Herdr Socket 自动探测失败

客户端先调用 Herdr 公共 CLI。默认 session 查询失败时，还会尝试：

```text
$HOME/.config/herdr/herdr.sock
```

命名 session 不猜测路径。如果仍然失败，在客户端配置中显式填写 `herdr.socket_path`。

## 安全提示

- 只在受信任内网部署当前版本。
- 不要把 Bot Secret、Cookie 或用户凭据提交到仓库。
- Herdr Pal 不会自动批准权限请求。
- Relay 断线期间不会缓存或补发旧任务。

## 开发与技术文档

查看版本：

```sh
./dist/herdr-pal-server --version
./dist/herdr-pal --version
```

运行完整检查：

```sh
./unittest.sh
```

更详细的架构、Herdr API 审计和维护上下文：

- [Bridge 架构](docs/BRIDGE_ARCHITECTURE.md)
- [Herdr API 审计](docs/HERDR_API_AUDIT.md)
- [维护交接](docs/HANDOFF_CONTEXT.md)
