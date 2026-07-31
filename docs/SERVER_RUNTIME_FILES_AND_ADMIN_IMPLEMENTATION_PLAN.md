# 服务端运行文件与管理体验实施计划

> **执行要求：** 在当前会话内逐项实施，不使用 subagent-driven。所有行为变更先写失败测试，再写最小实现。

**目标：** 将服务端运行文件迁移到独立目录，实时读取企微帮助，记录真实 Agent，并完善管理员改密和自动化接入说明。

**架构：** 配置包统一派生四个固定运行文件路径；Server 启动层负责创建引导与默认帮助文件；ConversationRouter 只依赖可替换的帮助读取接口。审计模型显式传递 Agent，Web 管理台只消费该字段。

**技术栈：** Go、标准库 HTTP/TLS/文件 API、OTLP Logs protobuf、内嵌 HTML/CSS/JavaScript、Loki HTTP API。

---

### 任务 1：迁移默认路径并删除 `addr_hint`

**文件：**
- 修改：`internal/config/path.go`
- 修改：`internal/config/server.go`
- 修改：`internal/config/relay_test.go`
- 修改：`cmd/herdr-pal-server/main_test.go`
- 修改：`cmd/hp-cli/main_test.go`
- 修改：`cmd/hp-cli/help_test.go`
- 修改：`server-config.example.json`

- [x] 增加失败测试，断言 Server 默认路径分别为 `server.json`、`auth.json`、`bootstrap.txt`、`help.md`，均位于 `~/.config/herdr-pal-server`，而 Pal 仍位于 `~/.config/herdr-pal/config.json`。
- [x] 运行 `go test ./internal/config ./cmd/herdr-pal-server ./cmd/hp-cli`，确认旧路径断言失败。
- [x] 将路径实现拆为客户端目录和服务端目录；在 `ServerConfig` 增加不参与 JSON 的 `RuntimeFiles`，并删除 `ListenerConfig.AddrHint`。
- [x] 更新 CLI 和配置测试，删除示例配置中的 `addr_hint`。
- [x] 重新运行目标测试并确认通过。

### 任务 2：持久化首次引导并实时读取帮助文件

**文件：**
- 新建：`internal/server/help.go`
- 新建：`internal/server/help_test.go`
- 新建：`internal/serverapp/runtime_files.go`
- 新建：`internal/serverapp/runtime_files_test.go`
- 修改：`internal/server/router.go`
- 修改：`internal/server/router_test.go`
- 修改：`internal/serverapp/app.go`
- 修改：`internal/serverapp/app_test.go`
- 修改：`internal/integration/web_admin_test.go`
- 修改：`internal/integration/admin_test.go`

- [x] 为 `FileHelpProvider.Read` 编写失败测试：每次读取磁盘最新内容，拒绝缺失、空文件和超过 64 KiB 的文件。
- [x] 为默认帮助文件和 `bootstrap.txt` 编写失败测试：原子创建、权限 `0600`、已有文件不覆盖。
- [x] 为 Router 编写失败测试：连续两次 `/help` 能观察到文件变更，读取失败返回明确提示并记录错误。
- [x] 运行 `go test ./internal/server ./internal/serverapp ./internal/integration`，确认测试失败。
- [x] 实现 `HelpProvider` 与文件读取器，将现有帮助文本改为不含动态地址的默认模板；Router 每次处理 `/help` 时调用读取器。
- [x] Server 启动时确保新目录、默认 `help.md` 和首次 `bootstrap.txt` 存在；测试覆盖文件路径注入，避免污染真实 HOME。
- [x] 删除 `buildRelayURLHint`、`RelayURL` 和帮助缓存字段，运行目标测试确认通过。

### 任务 3：让审计日志记录真实 Agent

**文件：**
- 修改：`internal/audit/event.go`
- 修改：`internal/audit/otlp.go`
- 修改：`internal/audit/otlp_test.go`
- 修改：`internal/server/router.go`
- 修改：`internal/server/router_test.go`
- 修改：`internal/lokiquery/model.go`
- 修改：`internal/lokiquery/client.go`
- 修改：`internal/lokiquery/client_test.go`
- 修改：`internal/webadmin/assets/static/app.js`
- 修改：`internal/webadmin/assets_test.go`

- [x] 增加失败测试，要求终端输出及可解析目标的用户输入事件携带 `Agent: "codex"`。
- [x] 增加 OTLP 和 Loki 失败测试，断言 `herdr_pal.audit.agent` 可写入并解析，历史记录缺失时保持空值。
- [x] 增加静态资源测试，断言审计 AGENT 列使用 `item.agent`，不使用 `pane_id` 回退。
- [x] 运行 `go test ./internal/audit ./internal/server ./internal/lokiquery ./internal/webadmin`，确认测试失败。
- [x] 在审计事件和 Loki 返回模型中加入 `Agent`；Router 从稳定会话元数据提取规范化 Agent，PaneID 仅保留诊断用途。
- [x] 运行目标测试确认通过。

### 任务 4：完善管理员自助改密

**文件：**
- 修改：`internal/webadmin/assets/templates/administrators.html`
- 修改：`internal/webadmin/assets/static/app.js`
- 修改：`internal/webadmin/auth_handler.go`
- 修改：`internal/webadmin/auth_handler_test.go`
- 修改：`internal/webadmin/assets_test.go`

- [x] 增加失败测试，要求管理员页面包含当前密码、新密码和确认密码表单，并且 JavaScript 对当前用户隐藏“重置密码”。
- [x] 更新接口测试，要求改密成功后撤销该用户全部会话、清除 Cookie，并要求重新登录。
- [x] 运行 `go test ./internal/webadmin`，确认测试失败。
- [x] 实现自助改密表单、确认密码校验和重新登录流程；其他管理员保留密码重置操作。
- [x] 运行目标测试确认通过。

### 任务 5：增加外部签发自动化接入指南

**文件：**
- 修改：`internal/webadmin/assets/templates/system.html`
- 修改：`internal/webadmin/assets/static/app.js`
- 修改：`internal/webadmin/assets/static/app.css`
- 修改：`internal/webadmin/assets_test.go`

- [x] 增加失败测试，要求系统页包含 Bearer Token、签发及删除接口、来源规则、一次性机器 Key 和动态基础 URL 提示。
- [x] 运行 `go test ./internal/webadmin`，确认测试失败。
- [x] 添加不包含真实 Token 的静态指南和 `curl` 示例，用 `window.location.origin` 填充当前 HTTPS 地址。
- [x] 运行目标测试确认通过。

### 任务 6：同步文档、全量验证并部署

**文件：**
- 修改：`README.md`
- 修改：`docs/HANDOFF_CONTEXT.md`
- 修改：`docs/WEB_ADMIN_CONSOLE_DESIGN.md`
- 修改：`docs/AUDIT_SERVICE_DEPLOYMENT.md`

- [x] 将服务端安装流程改为 `~/.config/herdr-pal-server`，说明 `help.md` 实时维护、`bootstrap.txt`、自助改密和系统页自动化指南；删除所有当前态 `addr_hint` 描述。
- [x] 运行 `gofmt`、`git diff --check` 和相关静态搜索，确认没有生产代码继续引用旧路径或 `addr_hint`。
- [x] 运行 `./unittest.sh`，预期全部通过。
- [x] 运行 `./build.sh`，预期生成当前系统及 Linux AMD64/ARM64 产物。
- [x] 提交实现后再次运行 `./build.sh`，确保二进制版本元数据对应干净 HEAD。
- [x] 远端备份旧配置和二进制，迁移四个运行文件，部署新 Server，验证企微、HPRP、Web 管理和 Loki。
