# 本地自动化测试补齐实施计划

> **执行方式：** 本项目禁止 subagent，本计划在当前会话中使用
> `superpowers:executing-plans` 逐项执行。测试覆盖现有行为时使用临时 mutation 验证测试确实
> 能捕获回归；任何临时 mutation 都必须在提交前恢复。

**目标：** 使用本机真实 Server、Pal、HPAP/HPRP 和 Unix Socket 补齐重启恢复、真实
`hp-cli`、自动分页与非法 Server Hello 回归测试。

**架构：** 跨组件测试复用 `internal/integration/admin_test.go` 的真实 Server harness，只把
企业微信和 Herdr 替换为本地 fake。纯协议与分页继续在所属包内测试，不为覆盖率新增生产
接口或修改协议语义。

**技术栈：** Go `testing`、`httptest`、Unix Socket、TLS/WSS、`os/exec`、现有 testkit。

---

## 任务 1：补齐 AdminClient Connection 与 Session 自动分页

**文件：**

- 修改：`internal/adminclient/pagination_test.go`

- [ ] 增加 `TestListConnectionsAggregatesPagesOnOneConnection`：本地 `net.Pipe` 连续返回两页
  `ConnectionListResult`，断言只 dial 一次、第二次请求携带第一页 token、条目顺序完整、
  `ObservedAt` 使用最后一页且最终 token 为空。
- [ ] 增加 `TestListSessionsAggregatesPagesOnOneConnection`，对 `SessionListResult` 做相同断言，
  同时确认原始 principal/machine filter 在下一页请求中保留。
- [ ] 把重复 token 测试改成 Key、Connection、Session 三种方法的表驱动测试，分别调用真实
  `ListKeys`、`ListConnections`、`ListSessions`。
- [ ] 运行：

  ```sh
  go test ./internal/adminclient -run 'Test(ListConnections|ListSessions|AutomaticPagination)' -count=1
  ```

  预期：新增测试通过。随后临时把 Connection 或 Session 聚合函数改成第一页后返回，确认对应
  测试失败，再立即恢复生产文件并重新确认通过。
- [ ] 完整门禁通过后提交：

  ```sh
  git add internal/adminclient/pagination_test.go
  git commit -m "test: 补齐管理列表自动分页覆盖"
  ```

## 任务 2：补齐非法 HPRP Server Hello

**文件：**

- 修改：`internal/hprp/validate_test.go`
- 修改：`internal/relayclient/client_hprp_test.go`

- [ ] 增加 `TestValidateServerHelloRejectsInvalidIdentityCapabilitiesAndLimits`，表驱动覆盖空
  connection ID、非法 machine ID、无版本 capability、消息限制为零和 heartbeat 为零。
- [ ] 增加 `TestHPRPClientRejectsInvalidServerHelloBeforeSnapshot`。fake TLS WebSocket 接收
  `hello.client` 后回复 machine ID 非法的 `hello.server`；服务端继续设置短读 deadline，
  断言没有收到 `session.snapshot`。
- [ ] Pal 使用最短退避运行到测试 context 超时；断言日志包含
  `stage=hello.server_decode`、`error_type=protocol`，且不包含 Bearer Key、用户 ID或原始 URL
  查询参数。
- [ ] 运行并确认：

  ```sh
  go test ./internal/hprp ./internal/relayclient -run 'Test(ValidateServerHello|HPRPClientRejectsInvalidServerHello)' -count=1
  ```

- [ ] 临时绕过 `hprp.ValidateServerHello` 调用，确认端到端测试会观察到非法快照发送并失败；
  恢复后重新运行聚焦测试。
- [ ] 完整门禁通过后提交：

  ```sh
  git add internal/hprp/validate_test.go internal/relayclient/client_hprp_test.go
  git commit -m "test: 覆盖 HPRP 非法服务端握手"
  ```

## 任务 3：覆盖 Server 重启而 Pal 保持运行

**文件：**

- 修改：`internal/integration/admin_test.go`

- [ ] 先新增 `TestHPAPServerRestartRestoresPalWithoutPalRestart`，引用尚未实现的 harness
  `stopServerForRestart` 和 `startServer`，运行并确认编译失败只因为缺少测试设施。
- [ ] 测试流程固定为：运行第一代 Server、签发 Key、启动一个 Pal、等待一条连接和会话；记录
  credential ID、connection ID 和稳定 target；仅停止 Server，确认 Admin Socket 删除、
  WeCom 连接归零且 Pal goroutine 尚未退出；在相同配置、监听地址、state dir 和凭据文件启动
  第二代 Server；等待 Pal 自动重连和会话恢复。
- [ ] 断言第二代 connection ID 与第一代不同，credential ID、principal、machine 和稳定 target
  不变；`connection list`、`session list` 均只返回一条，不出现重复机器或旧连接。
- [ ] 将 harness 的 Server 生命周期提取为独立代次：保存 config path，`startServer` 创建新的
  context/done，`stopServerForRestart` 接受 `context.Canceled`，最终 `stop` 仍只执行一次完整
  清理。Pal 生命周期保持原结构。
- [ ] 运行并至少重复三次：

  ```sh
  go test ./internal/integration -run TestHPAPServerRestartRestoresPalWithoutPalRestart -count=3
  ```

- [ ] 运行竞态测试：

  ```sh
  go test -race ./internal/integration -run TestHPAPServerRestartRestoresPalWithoutPalRestart -count=1
  ```

- [ ] 完整门禁通过后提交：

  ```sh
  git add internal/integration/admin_test.go
  git commit -m "test: 覆盖服务端重启后的 Pal 自动恢复"
  ```

## 任务 4：覆盖真实 hp-cli 进程

**文件：**

- 修改：`internal/integration/admin_test.go`

- [ ] 新增 `TestHPCLIProcessUsesRunningServer`，先引用未实现的 `buildHPCLI`、`runHPCLI` 测试
  helper，运行确认编译失败。
- [ ] `buildHPCLI` 在仓库根目录执行 `go build -o <t.TempDir>/hp-cli ./cmd/hp-cli`；
  `runHPCLI` 使用有界 context，显式传入 harness 的 `-config`，只继承必要环境，并捕获 stdout、
  stderr 与退出码。
- [ ] 通过真实子进程验证：`server status` 人工输出包含 HPAP/HPRP；`key list --json` 可解码且
  包含已签发 credential；Pal 上线后 `connection list --json` 和 `session list --json` 各包含
  一项；`key show 999999` 返回退出码 1 和稳定 `credential.not_found`，不是 transport 错误。
- [ ] 所有子进程输出断言不包含完整 Token、Bot Secret 或 fake Herdr 终端正文。
- [ ] 运行：

  ```sh
  go test ./internal/integration -run TestHPCLIProcessUsesRunningServer -count=1
  ```

- [ ] 完整门禁通过后提交：

  ```sh
  git add internal/integration/admin_test.go
  git commit -m "test: 添加 hp-cli 本地进程集成测试"
  ```

## 任务 5：最终覆盖率与稳定性验证

本任务不预期修改生产代码或设计文档。

- [ ] 连续运行重点集成场景：

  ```sh
  go test ./internal/integration -run 'Test(HPAPServerRestart|HPCLIProcess)' -count=3
  ```

- [ ] 运行完整门禁：

  ```sh
  ./unittest.sh
  ./build.sh
  ```

- [ ] 重新生成覆盖率并记录总覆盖率与关键包覆盖率，不把百分比作为通过门槛：

  ```sh
  go test -coverprofile=/tmp/herdr-pal-cover-after.out ./...
  go tool cover -func=/tmp/herdr-pal-cover-after.out | tail -1
  ```

- [ ] 执行敏感信息扫描和工作区检查：

  ```sh
  rg -n 'hpk_[A-Za-z0-9_-]{16,}|secret_sha256|terminal-sensitive' . \
    --glob '!**/*_test.go' --glob '!docs/*IMPLEMENTATION_PLAN.md'
  git diff --check
  git status --short
  ```

- [ ] 若设计文档无需调整，不额外制造文档提交；确认所有临时 mutation 已恢复且工作区干净。
