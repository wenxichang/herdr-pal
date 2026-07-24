# Herdr Pal

Herdr Pal 是 Herdr 与企业微信智能机器人之间的独立 bridge。当前网络模式由一个中央
`herdr-pal-server` 独占企业微信机器人连接，多台机器分别运行 `herdr-pal`，通过 WSS
把本机 Herdr 会话上报到服务端。用户在一个企业微信单聊里即可查看并操作自己所有在线
机器上的 Agent。

项目不修改 Herdr，不使用 MCP、plugin startup hook、私有 TUI socket 或内部 Rust
模块。普通文本只调用带状态等待的 `agent.prompt`，明确的 UI 操作只调用
`agent.send_keys`。

```text
企业微信单聊
      │ 唯一机器人长连接
      ▼
herdr-pal-server
      │ WSS Relay
      ├──────── herdr-pal（home-mac） ─── 本机 Herdr Socket
      └──────── herdr-pal（office-pc） ── 本机 Herdr Socket
```

## 能力与边界

- 服务端处理企业微信应用允许范围内的任意用户，只接受单聊文本。
- 同一用户可以连接多台机器；不同用户可以使用相同 `machine_id`。
- `(userid, machine_id)` 是在线连接唯一键，重复连接会拒绝后来者，避免两个客户端争抢。
- 客户端定期上报本机全部 Agent 会话；断线后服务端立即移除该机器及其会话。
- `/ls` 为当前用户聚合全部在线机器，编号只用于本次列表快照；实际选择保存稳定的
  `machine_id + pane_id + occupant`。
- 状态通知包含 `[机器/本地序号] Agent — panel 标题`，便于区分来源。
- Herdr、Relay 或企业微信断线后自动重连；运行状态只存内存，不补发断线期间的旧消息。
- `herdr-pal -i` 保留无 TUI 的本机控制台聊天模式，不需要 Relay 或企业微信。

终端内容来自 `agent.read` 的 `recent_unwrapped` 快照，可能混合用户输入、Agent 回复、
工具日志、spinner、状态栏和 TUI 重绘。它不是完整对话或结构化 LLM transcript；
alternate screen 中已经丢失的历史无法恢复。

首版不支持群聊、文件或语音输入、任意组合键、自动权限批准、持久化选择或离线消息重放。

## Herdr 兼容要求

Herdr Pal 精确支持审计过的 Herdr 公共 Socket API `protocol 17`。任何其他协议版本都会
进入 degraded 状态并停止读取和输入。

启动本机客户端前检查 Herdr：

```sh
herdr status server --json
```

输出必须显示服务正在运行且 `protocol` 为 `17`。命名 session 可用：

```sh
herdr session list --json
```

`PATH` 决定 Herdr Pal 通过公共 CLI 解析哪套 Herdr 配置和 Socket。调用
`agent.get`、`agent.read`、`agent.prompt` 和 `agent.send_keys` 时一律使用 pane ID；
terminal ID 和 occupant 用于防止输入落到被替换的 Agent。

## 构建

项目使用 Go 1.26：

```sh
./unittest.sh
./build.sh
```

`build.sh` 使用 `CGO_ENABLED=0`，生成两个单文件：

- `dist/herdr-pal-server`：中央企业微信与 Relay 服务端。
- `dist/herdr-pal`：每台 Herdr 机器上的 Relay 客户端，也提供 `-i` 交互模式。

查看版本：

```sh
./dist/herdr-pal-server --version
./dist/herdr-pal --version
```

## 配置和启动服务端

1. 在企业微信创建智能机器人，选择 API 模式的长连接接入，取得 Bot ID 和 Secret。
2. 让需要使用机器人的用户处于该应用允许范围内。
3. 复制 `server-config.example.json`，填写 Bot ID 和监听地址。
4. 只通过环境变量提供 Secret，然后手工启动服务端。

服务端配置示例：

```json
{
  "wecom": {
    "bot_id": "BOT_ID"
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

```sh
export HERDR_PAL_WECOM_SECRET='你的机器人 Secret'
./dist/herdr-pal-server -config /绝对路径/server.json
```

`cert_file` 和 `key_file` 必须同时填写或同时留空。留空时服务端在 `state_dir` 中生成并
复用自签名证书；`state_dir` 留空则使用用户配置目录下的 `herdr-pal-server`。私钥权限会
收紧为 `0600`。

服务端建立企业微信连接后，用户在自己的机器人单聊中发送：

```text
/userid
```

机器人会直接回复企业微信回调中的完整 `userid`。当前不再使用 `allowed_user_id`，也没有
公开的 `-discover-user` 模式。

## 配置和启动每台客户端

每台运行 Herdr 的机器复制 `config.example.json`，填写从 `/userid` 得到的值、该机器的
稳定标识和服务端地址：

```json
{
  "relay": {
    "url": "wss://192.168.1.10:9443",
    "userid": "USER_ID",
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

- `machine_id` 是用户可见的机器标识，建议使用稳定、简短且易区分的名称。
- 默认 Herdr session 时，`session` 和 `socket_path` 都留空；命名 session 填写
  `session`。
- 只有明确需要覆盖公共 CLI 解析结果时才填写本机 `socket_path`。
- `skip_verify` 默认就是 `true`，用于接受服务端自动生成的自签名证书；设为 `false` 时
  必须让系统信任服务端证书且证书名称匹配地址。
- `log.level` 支持 `debug`、`info`、`warn`、`error`。

启动客户端：

```sh
./dist/herdr-pal -config /绝对路径/client.json
```

需要保留日志时：

```sh
./dist/herdr-pal -config /绝对路径/client.json 2>&1 | tee herdr-pal.log
```

服务端和客户端都把连接、重连、拒绝、会话数量和安全错误写到 stderr，不会静默失败。
日志不记录 Secret、完整 prompt、Cookie 或完整终端快照。

## 企业微信命令

| 输入 | 行为 |
| --- | --- |
| `/userid` | 服务端直接显示当前企业微信 `userid` |
| `/ls` | 聚合当前用户全部在线机器上的 Agent，并建立本次编号快照 |
| `/1`、`/sel 1` | 从最近一次 `/ls` 选择编号 1 |
| `/help` | 显示输入帮助 |
| `/con` | 读取当前 Agent 最新 100 行，并重置到第 0 页 |
| `/pageup`、`/pagedn` | 上翻或下翻当前机器上的面板缓存 |
| `/enter`、`/key enter` | 发送 `enter` |
| `/key KEYS` | 发送一个或多个受限按键，结束后自动执行 `/con` |
| `/slash clear` | 将 `/clear` 作为普通 prompt 发送给当前 Agent |
| 其他不以 `/` 开头的文本 | 通过 `agent.prompt` 发送给当前 Agent |

`/userid`、`/ls`、选择和 `/help` 由服务端处理；其余输入原样转发到当前选择所在机器，
继续使用本地 Bridge 的状态、occupant、分页和按键策略。

`/key KEYS` 使用逗号或空白分隔，允许 `up`、`down`、`enter`、`esc`、`space` 或单个
ASCII 字母和数字；`dn` 是 `down` 的缩写，`sp` 是 `space` 的缩写。单条命令最多 32
个按键，相邻按键间隔 100ms。`enter` 只能单独发送，多键队列中包含 `enter` 会整组拒绝。

普通文本只允许发送给实时状态为 `idle` 或 `done` 的当前 occupant。`agent.prompt` 最多
等待约 5 秒的状态变化；若 Herdr 报告 stalled，会在复核 occupant、状态和序列后只补发
一次 Enter，再等待变化。按键是用户显式操作，不二次确认，但每次发送前都会重新校验目标。

## 列表、分页与通知

`/ls` 的条目示例：

```text
1. [home-mac/1] Codex — 修复登录测试
   工作区：backend / main
   状态：idle
2. [office-pc/1] Claude — 整理文档
   工作区：docs / main
   状态：working
```

全局编号会随在线机器和会话变化，只能与最近一次 `/ls` 配套使用。本地序号位于方括号中，
用于通知来源展示，不作为跨机器选择键。

- 逻辑页固定为 100 行。
- `/con` 读取最后 100 行并重置分页。
- `/pageup` 逐步扩大读取到最多 1000 行；`/pagedn` 只访问已有缓存。
- `blocked`、`done` 和需要输出的 `idle` 通知只读取最近 100 行，不读取全部新增内容，也
  不改变手工分页位置。
- pane 关闭、occupant 替换、Herdr 不可用或 Relay 断开时，会话从服务端目录移除，旧
  选择失效。

## 本机交互模式

不连接企业微信和 Relay，直接把控制台模拟成聊天框：

```sh
./dist/herdr-pal -i
./dist/herdr-pal -i -config /绝对路径/interactive.json
```

交互模式可不提供配置文件。需要覆盖 Herdr session、Socket 或日志级别时，仅配置
`herdr` 和 `log`。提示符、`[回复]` 和 `[通知]` 写到 stdout，结构化运行日志和按键审计
写到 stderr。`Ctrl+C`、`SIGTERM` 或 stdin 的 `Ctrl+D` 可正常退出。

## 测试

完整单元测试、静态检查和 race test：

```sh
./unittest.sh
```

多用户、多机器 Relay 端到端测试：

```sh
go test -race ./internal/integration -run TestRelayEndToEndRoutesMultipleUsersAndMachines
```

可选真实 Herdr 只读测试不访问企业微信外网：

```sh
PATH=/Users/wxc/Code/herdr/target/debug:$PATH \
HERDR_PAL_INTEGRATION=1 \
go test ./internal/integration -run '^TestRealHerdr$' -count=1 -v
```

## 安全模型

- Relay 只支持 WSS，不允许明文 WS，也不会把 Herdr 原始 Socket 暴露到网络。
- 当前 Relay 不验证客户端声明的 `userid`，默认跳过服务端证书校验，因此只适合受信任
  内网；不要直接暴露到互联网或不可信局域网。
- 企业微信应用自身的可见范围是用户入口边界；服务端只接受企业微信单聊文本。
- `(userid, machine_id)` 重复连接拒绝后来者；断线不保留离线执行队列。
- 企业微信 `msgid`、状态事件和通知均在内存中做有界幂等处理，进程重启后不恢复。
- 每次 prompt 或按键前重新校验 pane、terminal 和 occupant。
- 不提供 `server.stop`、`pane.close`、任意 `pane.send_input` 或自动审批入口。

架构、API 审计和已知限制见 [docs/BRIDGE_ARCHITECTURE.md](docs/BRIDGE_ARCHITECTURE.md)、
[docs/HERDR_API_AUDIT.md](docs/HERDR_API_AUDIT.md) 和
[docs/HANDOFF_CONTEXT.md](docs/HANDOFF_CONTEXT.md)。
