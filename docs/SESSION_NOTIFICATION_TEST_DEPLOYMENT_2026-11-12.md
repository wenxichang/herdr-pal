# 通知开关测试服务器部署记录

目标：`121.46.26.51:4222`（SSH），用户 `wxc`。仅部署此测试机；`hp-internal` 未连接、未部署，等待用户测试完成后的反馈。

## 再次审核与交付物

- 再次检查业务组合隔离、批量部分成功、空列表保留、每机器非空清理、done 读取前过滤、主动查询与命令输出隔离，未发现新的阻断项。
- 本轮 `go test -race ./internal/notificationprefs ./internal/server ./internal/serverapp ./internal/integration -run 'TestStore|TestIdentity|TestNotification|TestRouter.*(Muted|Pruned|Reenable|List|Notification)|TestRunRejectsInvalidNotificationPreferences' -count=1` 通过。
- 交付物来自前轮已通过完整 `unittest.sh` 与 `build.sh` 的构建；部署前源码指纹再次核对一致，本轮未修改 Go 生产代码。
- 测试构建：`v0.8.0-dirty`，commit `7b8020c`，构建时间 `2026-11-12T14:45:40Z`。包含当前功能分支未提交改动，不是新的公开 Release。
- Linux AMD64 服务端 SHA-256：`52ecfb9c5f80882faa9da82e82d21c399ab850a9c16b96666629497b65736ae9`。
- cmd/internal/go.mod/go.sum 工作区源码指纹：`1d5bba2ed44f5d7a1b8b9271ed6519f0af5f56224925ab9a5d4f3bbe82c67609`。

## 部署内容

- 运行路径：`/home/wxc/dist/herdr-pal-server-linux-amd64`。
- 保留原有后台启动方式、参数、工作目录、进程环境及日志位置；没有安装新的系统服务。
- 保留配置、管理员认证、机器凭据与 TLS 证书/私钥；部署后逐文件摘要核验未变。
- 在原有自定义帮助中仅补入通知命令段和对应定向命令限制，保留安装地址等既有内容；帮助文件权限为 `0600`。
- 当前备份：`/home/wxc/.config/herdr-pal-server/backups/notification-20261112T150648Z`，包含旧二进制及相关运行文件；回滚时只恢复二进制与本次修改的帮助，不覆盖用户后续产生的凭据或状态变更。
- 部署与验证结果：`/home/wxc/dist/.deploy-notification-20261112T150648Z/result.json`、`/home/wxc/dist/.deploy-notification-20261112T150648Z/verification.json`。

## 部署过程中的检查脚本修正

首次检查未处理 Pal 重连期间 `connection.list.items=null` 的合法过渡状态，触发回滚。随后独立确认旧版本已恢复、企微连接正常、1 台 Pal 和 10 个会话均恢复。

部署脚本已将 null 与空数组统一视为空连接列表，并对 null、空数组、就绪客户端三种输入补充检查；重新部署成功。业务代码未因该检查脚本错误发生修改。

## 独立健康验证

部署控制脚本退出后，通过新的 SSH 连接连续检查三次：

- PID：`1756733`，版本与构建时间匹配。
- 企业微信：`connected`。
- 原有 Pal：`1` 台已重新连接，目录恢复 `10` 个会话。
- Relay 监听：`[::]:4000`；Web 管理监听：`[::]:4001`；Web 页面 HTTP 状态 `200`。
- 运行中进程的可执行文件路径与已核验摘要一致。
- `server.json`、`auth.json`、`credentials.json`、TLS 证书与私钥的摘要与部署前备份一致。
- 远端帮助已包含 `/notify off 1,2 3` 和非空清理条件。

没有向用户真实 Agent 发送输入、审批或测试消息。首次 `/ls` 或 `/notify` 写入时才会创建通知偏好文件；功能交互由用户在企微中测试。服务重启后应先 `/ls` 刷新会话编号。


后续：用户已确认测试通过，通知状态已调整为 `✔️ 开启` / `✖️ 关闭` 并部署到 hp-internal，详见 [内网部署记录](SESSION_NOTIFICATION_INTERNAL_DEPLOYMENT_2026-11-12.md)。本文件保留测试机当时的部署信息。
