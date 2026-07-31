### Herdr Pal 快速上手

已经完成安装时：`/ls` 查看会话 → `/1` 选择会话 → 直接发送任务。

【基本控制】
`/userid` 获取当前企业微信用户 ID
`/ls` 列出所有在线机器和 Agent
`/N` 或 `/sel N` 选择第 N 个会话
`/N 内容` 在第 N 个会话执行，成功后切换；`#N 内容` 执行但不切换
`/con` 查看当前会话最近 100 行
`/pageup`、`/pagedn` 上下翻页
`/mode img` 当前会话使用终端图片，保留颜色和选中样式
`/mode txt` 当前会话使用纯文本
`/slash clear` 向 Agent 发送 `/clear`
普通文字直接发送给当前 Agent
`/help` 显示本帮助

OpenCode 默认使用图片模式，其他 Agent 默认使用文本模式；模式只在 Server 本次运行期间保存。
定向前缀不能用于 `/userid`、`/ls`、`/help`、`/N` 或 `/sel N`。

【按键操作】
`/key up`、`/key down` 发送方向键
`/key space`、`/key esc` 发送空格或 Esc
`/enter` 等同 `/key enter`
`/key down,sp,dn,A,7` 连续发送多个按键

按键可用逗号或空格分隔，最多 32 个；`dn` 表示 down，`sp` 表示 space，也支持单个英文字母和数字。按键间隔 100ms，完成后自动返回终端内容。Enter 只能单独发送。

【一键安装：Linux / macOS】

1. 发送 `/userid`，把返回的用户 ID 交给管理员，获取本机专用的 Server URL 和机器 Key。
2. 从以下地址下载最新版本：
   https://github.com/wenxichang/herdr-pal/releases/latest
3. 选择与本机匹配的 `herdr-bundle-<版本>-<系统>-<架构>.tar.gz`：
   - Apple Silicon：`darwin-arm64`
   - Intel Mac：`darwin-amd64`
   - Linux x64：`linux-amd64`
   - Linux ARM64：`linux-arm64`
4. 使用同名 `.sha256` 文件校验下载内容，解压后进入目录并执行：

   `./install.sh`

5. 按提示选择安装目录，默认 `~/.local/bin`；然后输入 Server URL 和机器 Key。Key 不会显示在终端。
6. 安装器会备份旧文件、配置 Herdr Sidecar，并在 Herdr 正在运行时询问是否立即 `live-handoff`。
7. 启动或切换 Herdr 后回到企业微信，发送 `/ls` 验证接入。

正常安装不需要手工编辑配置。高级用户可以检查：

- Herdr Pal：`~/.config/herdr-pal/config.json`
- Herdr：`~/.config/herdr/config.toml`

Herdr 与 `herdr-pal` 会安装在同一目录。Herdr 启动后由 Sidecar 在其生命周期内自动启动、守护并停止 Herdr Pal。
