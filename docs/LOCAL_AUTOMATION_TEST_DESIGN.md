# 本地自动化测试补齐设计

## 1. 目标

在不依赖真实企业微信、真实 Herdr、Linux 主机或 Windows 主机的前提下，补齐当前最重要的
本地自动化回归缺口。测试优先验证用户已经遇到过的故障和跨组件契约，不以单纯提高覆盖率
数字为目标。

## 2. 范围

本轮增加四组测试：

1. Server 重启而 Pal 不重启：使用真实 `serverapp`、TLS/WSS、CredentialStore、HPRP Pal
   和 SessionCatalog，使用 fake WeCom、fake Herdr。停止第一代 Server 后保持 Pal 运行，在
   相同监听地址、状态目录和凭据文件上启动第二代 Server，确认 Pal 自动重连、重新认证、
   重新上报完整会话，且管理面的 `connection list`、`session list` 恢复。
2. `hp-cli` 真实进程：构建临时 `hp-cli` 可执行文件，通过真实 HPAP Unix Socket 调用运行中
   Server，验证 `server status`、列表查询、`--json` 和业务错误退出码。测试只传配置路径，
   不向子进程传企业微信 Secret。
3. AdminClient 自动分页：分别为 Connection 和 Session 构造两页响应，确认使用同一连接、
   正确传递 page token、聚合全部条目并保留最后观测时间；同时覆盖重复 token 拒绝。
4. HPRP 非法 Server Hello：由本地 fake WebSocket Server 返回身份、限制或能力无效的
   `hello.server`，确认 Pal 不进入 READY、不发送会话快照，并记录安全的协议阶段错误后重试。

## 3. 固定边界

- 不访问公网和企业微信正式地址。
- 不向真实 Herdr 发送 prompt 或按键。
- 不为测试修改 HPRP、HPAP 或 Herdr 的生产语义。
- 不为模拟 Linux peer credential 或 Windows Named Pipe 重构系统调用边界；这些仍由目标平台
  测试负责。
- 测试日志、失败信息和子进程输出不得包含完整机器 Key、Bot Secret、prompt 或终端正文。
- 所有等待使用有界 deadline 和条件轮询，不使用长时间固定 sleep。

## 4. 测试结构

- Server 重启测试扩展 `internal/integration/admin_test.go` 的 HPAP harness，使 Server 代次可
  独立启动和停止，而 Pal 生命周期独立于 Server。第一代停止后的 Admin Socket 必须删除；
  第二代 READY 后连接 ID 应变化，会话稳定目标和凭据 ID 保持一致。
- `hp-cli` 进程测试放在 `internal/integration`，复用同一 Server harness；通过 `go build`
  生成临时二进制，并用 `exec.CommandContext` 捕获 stdout、stderr 和退出码。
- 分页测试只修改 `internal/adminclient/pagination_test.go`，使用现有 `net.Pipe` fake，不引入
  新生产接口。
- 非法 Server Hello 优先在 `internal/relayclient` 使用本地 TLS WebSocket fake，复用现有
  日志和连接等待辅助函数，不把协议异常逻辑放入 testkit。

## 5. 成功标准

- 新测试在缺少相应测试设施或当前行为不满足断言时先失败，再通过最小改动转绿。
- Server 重启测试至少连续运行三次无时序失败。
- `go test -race` 下没有数据竞争。
- `./unittest.sh` 和 `./build.sh` 全部通过。
- 默认测试仍不要求设置 `HERDR_PAL_INTEGRATION` 或访问外部网络。

## 6. 明确保留的缺口

- 真实 Herdr 的只读和 live prompt 测试继续由 `HERDR_PAL_INTEGRATION`、
  `HERDR_PAL_LIVE_INPUT` 显式开启。
- Linux peer UID、Windows Named Pipe 和目标平台进程生命周期需要后续在对应操作系统运行。
- 企业微信官方协议变化仍需部署环境联调，fake 只验证当前适配器契约。
