# 通知开关 emoji 与 hp-internal 部署记录

用户已确认测试机通知功能可用，按既定安排将通知功能及 `✔️ 开启` / `✖️ 关闭` 显示部署到 `hp-internal`。

## 修改与验证

- `/ls` 和 `/notify` 查询统一通过 notificationStateLabel 显示 emoji 与文字；开关存储和过滤语义不变。
- 已先观察 emoji 标签及列表/查询回归失败，再更新实现并通过 Server 竞态检查。
- `./unittest.sh` 通过，包含普通集成测试与竞态集成测试。
- `./build.sh` 通过；构建 `v0.8.0-dirty`，时间 `2026-11-12T15:44:34Z`，commit `7b8020c`，包含功能分支未提交改动。
- Linux AMD64 SHA-256：`0c71fc382edd632b09d3865169b3aae04f2cdafa76ce42efcbebb2fc9a8d82b1`。
- 工作区构建输入指纹：`d7a036ef912e38452a58304c6a1f8e7ae5092fbba72481cf10e1c89393564bab`；部署后再次核对一致。

## 服务器原始帮助保护

- 帮助源文件为服务器 `/home/wxc/.config/herdr-pal-server/help.md`，没有上传或使用仓库 help.md 覆盖。
- 先备份服务器原始文件，再在其现有“按键（Herdr）”段前插入通知说明，保留原有常用命令、dsh 说明、安装地址等全部原文。
- 独立核验：删除本次新增段落后，文件字节与服务器原始备份完全一致。
- 修改前 SHA-256：`36aeecf4478a3d546a32cd5c84747259a2adec00c74b3690c1372f6fac066226`。
- 修改后 SHA-256：`0a15e650a579a6c6f93678323f9185d80af0dfe03b958d4f0e5b848c6fba8c07`。
- 修改后权限：`0600`。

## 部署与回滚

- 目标：`hp-internal`，SSH 用户 `wxc`，端口 22。
- 沿用原 systemd 服务 `herdr-pal-server-v2.service`，未修改服务文件和启动环境。
- 二进制：`/home/wxc/herdr-pal-server-v2/herdr-pal-server`。
- 备份：`/home/wxc/.config/herdr-pal-server/backups/notification-emoji-20261112T154803Z`。
- 暂存与结果：`/home/wxc/herdr-pal-server-v2/.deploy-notification-emoji-20261112T154803Z/result.json`、`/home/wxc/herdr-pal-server-v2/.deploy-notification-emoji-20261112T154803Z/verification.json`。
- 第一次尝试发现服务器尚无备份父目录，在停止服务之前退出；核验原 PID 和 32 台 Pal 均未受影响。补充创建私有备份目录后重试成功。
- 回滚只恢复旧二进制和本次修改的服务器帮助，不覆盖后续用户凭据或通知设置。

## 独立健康检查

- systemd：active / running，PID `1867766`。
- 企微：connected。
- 原有 32 台 Pal 全部重连，缺失客户端数 0。
- 当前目录 583 个会话；部署前为 585，使用各 Pal 重连后的最新权威快照，不强制保留旧目录。
- 新 SSH 连接下连续三次检查均正常，运行路径和二进制摘要匹配。
- Web 管理页 HTTP 200；Relay 4000、Web 管理 4001 监听不变。
- server.json、auth.json、credentials.json 和 TLS 证书/私钥与备份摘要一致。

没有向用户真实 Agent 发送输入、审批或测试消息。未提交、合并或推送 Git。
