# Herdr Pal

Herdr Pal 是运行在本机的独立 IM bridge。首个版本使用 Go 编译为单文件，通过 Herdr
公共 Local Socket API 与企业微信智能机器人长连接，把 Agent 状态通知、终端近期快照
和受限输入带到自己的企业微信单聊中。

它不修改 Herdr，不使用 MCP、plugin startup hook、私有 TUI socket 或内部 Rust 模块。
普通文本只调用 `agent.prompt`，明确的 UI 操作只调用 `agent.send_keys`。

## 能力与边界

- 只接受配置中的一个企业微信用户，且只处理单聊。
- 监控当前 Herdr 会话中的全部 Agent pane。
- 支持 Agent 列表、目标选择、最近终端内容分页、普通 prompt 和受限按键。
- `working`、`blocked`、`done`、`idle`、`unknown` 状态按策略主动通知。
- Herdr 或企业微信断线后自动重连；运行状态只存内存，不补发断线期间的旧消息。
- 手工启动和停止，不安装 `launchd`、systemd 或容器服务。

终端内容来自 `agent.read` 的 `recent_unwrapped` 快照，可能混合用户输入、Agent 回复、
工具日志、spinner、状态栏和 TUI 重绘。它不是完整对话，也不是结构化 LLM transcript；
alternate screen 中已经丢失的历史无法由 Herdr Pal 恢复。

首版不会自动批准任何权限请求，不支持群聊、多用户、任意组合键、文件或语音输入，也
不会把 Herdr Socket 暴露到远程网络。

## 兼容要求

Herdr Pal 精确支持审计过的 Herdr 公共 Socket API `protocol 17`。不是“17 或更高”，
任何其他协议版本都会进入 degraded 状态并停止读取和输入。

启动前检查默认 Herdr 服务：

```sh
herdr status server --json
```

输出必须显示服务正在运行且 `protocol` 为 `17`。命名 session 可用：

```sh
herdr session list --json
```

当前开发机安装的 Herdr 0.7.1 返回 `protocol: 14`，因此仓库中的真实 Herdr 集成测试
会跳过；fake 端到端测试完整使用 protocol 17。不能据此宣称已在本机完成真实联调。

## 企业微信配置

1. 在企业微信创建智能机器人，选择 API 模式的长连接接入，取得 Bot ID 和 Secret。
2. 用自己的企业微信帐号向机器人发送过至少一条消息。企业微信要求满足这一前置条件
   后，机器人才能向该单聊主动推送状态通知。
3. 从真实消息回调确认 `userid`，把同一个标识写入 `allowed_user_id`。若平台返回加密
   userid，就使用对应的加密值。
4. Secret 只放入环境变量，不写入 JSON、日志或仓库。

企业微信长连接协议文档：
[智能机器人长连接](https://developer.work.weixin.qq.com/document/path/101463)。

复制并填写配置：

```json
{
  "wecom": {
    "bot_id": "BOT_ID",
    "allowed_user_id": "USER_ID"
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

- 默认 Herdr session 时，`session` 和 `socket_path` 都留空。
- 使用命名 session 时填写 `session`。
- 只有明确需要覆盖公共 CLI 解析结果时才填写本机 `socket_path`。
- `log.level` 支持 `debug`、`info`、`warn`、`error`。

设置 Secret：

```sh
export HERDR_PAL_WECOM_SECRET='你的机器人 Secret'
```

## 构建与运行

项目使用 Go 1.26：

```sh
./unittest.sh
./build.sh
```

`build.sh` 使用 `CGO_ENABLED=0`，生成单文件 `dist/herdr-pal`。

查看版本：

```sh
./dist/herdr-pal --version
```

手工启动：

```sh
./dist/herdr-pal -config /绝对路径/config.json
```

按 `Ctrl+C` 停止，也可以发送 `SIGTERM`。程序会停止接收新消息、取消 Herdr 请求、
关闭两侧连接并释放同 Bot 的本机进程锁；优雅退出上限为 10 秒。

## 单聊命令

| 输入 | 行为 |
| --- | --- |
| `/ls` | 列出当前 Agent，并建立本次编号快照 |
| `/sel 1` | 从最近一次 `/ls` 选择编号 1 |
| `/con` | 读取当前 Agent 最新 100 行，并重置到第 0 页 |
| `/pageup` | 向更早内容移动一页 |
| `/pagedn` | 从缓存向更新内容移动一页，不读取 Herdr |
| `/keyup`、`/key up` | 发送 `up` |
| `/keydn`、`/key down` | 发送 `down` |
| `/enter`、`/key enter` | 发送 `enter` |
| `/esc`、`/key esc` | 发送 `esc` |
| `/space`、`/key space` | 发送 `space` |
| `/key X` | 发送一个 ASCII 字母或数字，保留大小写 |
| 其他不以 `/` 开头的文本 | 通过 `agent.prompt` 发送给当前 Agent |

`/key X` 只接受单个 `A-Z`、`a-z` 或 `0-9`；特殊键名只接受表中的小写形式。不支持
组合键、控制键、任意 key name 或按键序列。命令本身就是用户的显式操作，不二次确认，
但 `blocked` 状态永远不会自动发送 Enter 或空格。

以 `/` 开头但无法识别的内容只返回命令错误，绝不会作为 prompt 转发。

## 分页与状态通知

- 逻辑页固定为 100 行。
- `/con` 调用 `agent.read(recent_unwrapped, 100)`，显示最后 100 行并重置分页。
- `/pageup` 依次读取 200、300，直到最多 1000 行，即最多访问最近 10 页。
- `/pagedn` 只使用当前缓存；需要刷新最新内容时再次执行 `/con`。
- 扩大读取后若旧缓存无法与新快照稳定对齐，分页会清空并提示重新 `/con`。
- 超过企业微信 Markdown 大小的逻辑页按 UTF-8 安全边界分段。
- `blocked`、`done` 以及需要输出的 `idle` 通知始终只读取最近 100 行，不读取全部新增
  内容，也不改变用户的手工分页位置。

pane 关闭、Agent occupant 替换或 Herdr 重连都会使旧选择失效；继续输入前必须重新执行
`/ls` 和 `/sel N`。

## 测试

完整单元测试、静态检查和 race test：

```sh
./unittest.sh
```

fake Herdr + fake 企业微信端到端测试：

```sh
go test ./internal/integration -run TestBridgeEndToEnd
```

可选真实 Herdr 测试仍使用 fake 企业微信，不访问企业微信外网，也不需要真实 Secret：

```sh
HERDR_PAL_INTEGRATION=1 go test ./internal/integration -run TestRealHerdr -v
```

该测试首先执行 `herdr status server --json`。服务未运行、CLI 不可用或协议不是 17 时
会明确 `Skip`；只有通过门禁后才读取真实 `session.snapshot`。

## 安全模型

- 默认拒绝未知用户、群聊、未知 pane 和失效 occupant。
- 企业微信 `msgid` 使用有容量和 TTL 上限的内存集合去重。
- 每次 prompt 或按键前重新校验 pane、terminal 和 Agent occupant。
- 每次已选目标的白名单按键尝试都会写入结构化审计，只包含用户、pane、occupant 摘要、
  规范化按键、时间和 `sent`/`rejected`/`failed` 结果；重复消息、未授权输入和原始非法
  命令不产生按键审计。
- 日志只记录连接状态、安全错误类别、长度或摘要，不记录 Secret、完整 prompt、Cookie
  或完整终端快照。
- 没有 `server.stop`、`pane.close`、`pane.send_text`、`pane.send_input` 或自动审批入口。

架构、API 审计和已知限制见 [docs/BRIDGE_ARCHITECTURE.md](docs/BRIDGE_ARCHITECTURE.md)
与 [docs/HERDR_API_AUDIT.md](docs/HERDR_API_AUDIT.md)。
