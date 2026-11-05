# Herdr 0.9.5 兼容与 Bundle 发布计划

**目标：** 只修改 herdr-pal，兼容官方 Herdr v0.9.5，并发布四个平台的新版 Bundle。

**设计：** 公共 Socket 协议仍为 22，保留 17–22 门禁。官方 Herdr 不支持旧 fork 的
`[[sidecar]]`，默认安装只生成 Pal 配置，用户手动启动 Pal；不安装系统用户服务。
迁移时仅移除旧安装器标记的 Pal sidecar 块，保留其他配置并交给官方 CLI 校验。
显式 `setup --startup sidecar` 保留旧 fork 部署方式。

**约束：** 不修改或发布 Herdr 仓库，不触碰现有运行会话；仅通过公开 API 做隔离测试。
凭据不进入包、服务参数或日志；提交前运行完整构建和单测。按项目约定由当前执行者完成，
不使用 subagent-driven development。

## 实施步骤

- [x] 在 `cmd/herdr-pal/setup_test.go` 复现官方 Herdr 拒绝 sidecar 导致的安装失败。
- [x] 在 `internal/installer` 实现默认手工启动配置与受管 sidecar 迁移；覆盖幂等、无关
  配置保留、非法标记拒绝和写入失败回滚。
- [x] 更新 `packaging/bundle/install.sh` 和文档，明确安装后手动启动 Pal，保留可选 Herdr
  live-handoff，不把它描述成启动 Pal 的手段。
- [x] 在隔离 Herdr v0.9.5 实例验证 ping、snapshot、订阅、Agent 读取及安装配置校验；
  测试不访问 IM 网络，不向用户现有 Agent 发送输入。
- [x] 执行 `./unittest.sh`、`./build.sh` 并自审变更。
- [ ] 提交中文 conventional commit。
- [ ] 以 v0.6.2 构建 Pal，使用官方 v0.9.5 四平台二进制（核验上游 SHA-256）生成 Bundle，
  检查架构、归档内容、版本及校验和。
- [ ] 推送 herdr-pal main 和 annotated tag，发布四个 Bundle 与四个校验文件，复核远端。

## 审计依据

- 官方最新正式版：v0.9.5，提交 `2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01`。
- 最新主分支：`ee5bfb5041bb3e4a49d591c72308aae3c81ecdde`。
- 两者生成的公共 JSON Schema 一致，protocol 22 / schema_version 1。
- 官方二进制 `config check` 对旧配置报错：`unknown config section [[sidecar]]`。
- 发布只使用官方正式版二进制；主分支检查不等同于主分支真实运行验证。

## 审查重点

受管标记损坏时不得删除任意配置；未受管 sidecar 不得静默丢弃；配置写入失败必须恢复旧
内容；安装提示必须引用真实安装路径；测试与发布不得改变用户当前 Herdr 或 Pal 的部署。

## 执行记录

- 官方安装失败的回归测试先失败，修复后通过；安装提示回归测试同样完成失败到通过。
- 隔离 v0.9.5 公共 API 与交互模式测试通过。
- 自审补充 TOML 作用域检查：标记外仍属于 sidecar 的字段不能迁移成根配置或其他表字段。
- 首轮全量测试发现两项帮助文本断言仍要求 Sidecar，已同步为手动启动说明。
- 全量普通与竞态测试共 70 个包次通过，`./build.sh` 完成全部平台构建。
- 真实 macOS ARM64 安装迁移验证通过，macOS x64 官方二进制版本检查通过。
