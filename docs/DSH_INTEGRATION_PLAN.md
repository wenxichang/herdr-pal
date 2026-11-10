# dsh 插件接入设计与实施计划（历史，已废弃）

> **已被取代（2026-11-10）**：本文记录旧的 Node 插件方案。Pal 现已改为直接连接 dsh
> 本机 Remote API，`plugins/dsh-herdr-pal/` 与 `internal/dshlegacy/` 均已删除，`build.sh`
> 不再构建或打包插件。当前实现见 [原生 API 设计](DSH_NATIVE_API_DESIGN.md) 和
> [交接上下文](HANDOFF_CONTEXT.md)。以下内容仅用于保留历史决策记录。

目标：在本仓库提供第三方 dsh 插件，让企微沿用 `/ls`、`/N`、普通消息和 `/con` 操作 dsh。

## 设计

针对本机已安装的 dsh 0.2.1-rc.5，使用公开 Cordis 服务 `agents`、Agent `followup` 和 Session `snapshotEvents`。插件暴露当前已加载根会话和最近 14 天的历史会话。通过公开 sessionController 列表与恢复接口、sessionQuery.readSession 无副作用读取原始历史，不触碰审批接口。插件放在 `plugins/dsh-herdr-pal`，可通过 dsh 插件包管理安装。

插件提供 HTTP JSON v1 接口，固定监听 127.0.0.1，要求至少 32 字节 Bearer token，拒绝浏览器 Origin、限制正文和响应大小。会话身份包含每次插件加载及 Agent 实例的随机 incarnation，防止重启或同 ID 替换后误发；消息幂等键绑定 incarnation 与原文。失败不自动重发。

Pal 新增独立 `internal/dsh` 客户端与执行器，组合执行器将 Herdr 和 dsh 合并到既有 HPRP 目录。企微 Router、用户身份、机器 Key、消息去重沿用已有链路。dsh 仅提供纯文本输出；按键明确拒绝，审批继续留在 dsh。运行状态变化通过现有 notification 机制触发内容回传。

配置新增可选 `dsh.url/token` 和 `herdr.disabled`。默认保留原行为；可只启用 dsh，不要求安装或启动 Herdr。多个 dsh 进程本轮使用不同端口并分别配置 Pal，单个 Pal 配置一个 dsh 端点。交互 `-i` 本轮仍用于 Herdr，目标是企微链路。

## 实施步骤

- [x] 插件协议测试：认证、列表、稳定身份、消息幂等冲突、忙碌拒绝、文本清理、实例替换；随后实现插件与 bundle manifest。
- [x] Go 客户端/执行器测试：目录刷新、掉线撤下、身份检查、只发送一次 prompt、读取结果与通知；随后实现 `internal/dsh`。
- [x] 组合执行器与配置测试：Herdr/dsh 混合列表和路由、无 Herdr 模式、旧配置兼容；随后接入 `internal/app`。
- [x] 通过本地 fake 和真实 Cordis/DSH 服务验证，不调用 LLM 或企微网络；更新安装说明、build.sh 与 unittest.sh。
- [x] 运行全部单测（含 race）、全平台构建，检查最终差异。

## 验证重点

断线不得继续显示可操作旧会话；同 ID Agent 替换不能继承选择；审批期间不把文本当批准；超时不能自动重试输入；插件 token 不进入日志或 Git。

## 执行记录

按项目要求由当前会话直接实现，不使用 subagent-driven。用户已要求创建插件并接入，工作保留在当前工作区供审阅，不自动提交或发布。用户补充要求包含两周内历史会话，按 dsh updatedAt 过滤，首次发消息按需恢复，恢复后保持稳定目标。

验证记录：9 项 Node 插件测试通过；生产 Pal/HPRP/企微 Router 本地集成通过；隔离真实 dsh 加载、持久化历史列表与读取通过。`./unittest.sh` 完成全量 Go 单测及 race 检查。构建脚本测试夹具已增加插件文件；请求取消后的迟到提交由回归测试覆盖。
`./build.sh` 通过，生成所有既有平台二进制及 `dist/dsh-herdr-pal-0.1.0.tgz`，包内包含入口、协议实现、bundle manifest 和中文说明。

0.1.1 输出范围修正：按用户要求保留聊天正文，用户输入必须 source.kind=user；助手只取 text 块；只使用原始 append 事件，不把模型上下文、压缩替换、工具或推理作为聊天内容。过滤后再限制最近 100 条，沿用现有分页。
