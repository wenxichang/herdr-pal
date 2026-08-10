# Herdr 0.8.0 Bundle 发布设计

## 目标

将官方 Herdr `v0.8.0` 合入 `github.com/wenxichang/herdr`，保留 fork 中的 CSC、Sidecar
守护和图形流修复；使用合并后的 Herdr 与当前 herdr-pal 构建 macOS、Linux 四个平台的
Bundle，并以 herdr-pal `v0.6.0` 发布。

## 仓库和发布边界

- Herdr fork：在 `master` 合入官方 `v0.8.0`，完成测试后推送 `origin/master`。
- Herdr fork 不创建新 Tag，不创建 GitHub Release。
- herdr-pal：在 `main` 完成必要文档或打包修正，创建并推送 `v0.6.0`。
- herdr-pal GitHub Release 只发布四个平台 Bundle 及各自 SHA-256 文件。
- 不发布独立 Pal、Server、hp-cli 或 Windows 资产。

## Herdr 合并策略

采用普通 merge commit 把官方 `v0.8.0` 合入 fork `master`，不 rebase、不改写已有历史，
也不强推。

冲突解决遵循以下优先级：

1. 以官方 `v0.8.0` 的运行时、协议、输入和集成框架为基础。
2. 重新嵌入 fork 的 `csc` IntegrationTarget、检测 manifest、状态脚本和恢复能力。
3. 保留 `[[sidecar]]` 配置、启动、生命周期守护和测试。
4. 保留图形流注册顺序修复；若官方已经等价修复，则采用官方实现并删除重复补丁。
5. 生成的 API Schema 必须来自最终合并代码，不能手工保留旧 protocol 17 内容。
6. 文档冲突保留官方 0.8.0 内容，并补入 fork 独有的 CSC 和 Sidecar 说明。

合并提交使用中文 conventional commit，例如：

```text
merge: 合入 Herdr v0.8.0
```

## Pal 和 Bundle

herdr-pal 已显式允许 Herdr protocol 17、19。本次还需检查 README、安装提示和 Bundle
模板，不得继续把 protocol 17 描述为唯一版本。

四个发布目标为：

- `darwin-arm64`
- `darwin-amd64`
- `linux-amd64`
- `linux-arm64`

每个压缩包必须同时包含：

- 合并后 Herdr fork 的目标平台二进制。
- herdr-pal `v0.6.0` 的目标平台二进制。
- `install.sh`。
- Bundle `README.md`，记录 Herdr 提交。

## 验证策略

### Herdr

- 运行 `just check`。
- 运行 fork 的 `unittest.sh`，确认 CSC、Sidecar 和 headless/live-handoff 定向测试。
- 确认最终 API Schema 的 protocol 为 19。
- 确认 CSC manifest、IntegrationTarget 和 Sidecar 配置仍存在。
- 构建四个平台的 release 二进制；本机可执行的 macOS ARM64 版本需验证版本和公共 CLI。

### herdr-pal

- 运行 `./unittest.sh`，包含竞态检查。
- 运行 `./build.sh`。
- 运行 protocol 19 的 HerdrClient、Supervisor、快照和真实状态门禁测试。
- 使用合并后的 Herdr 做本机只读联调，至少覆盖 `ping`、`session.snapshot`、事件订阅和
  `herdr-pal -i` 的连接初始化，不访问企业微信网络。

### Bundle

- 构建四个平台压缩包及 SHA-256 文件。
- 对每个压缩包执行校验和复核、解包和文件清单检查。
- 使用 `file` 验证 Herdr 与 Pal 的目标架构；Linux 二进制必须为静态链接。
- 本机 macOS ARM64 Bundle 需运行 `herdr --version`、`herdr api schema --json` 和
  `herdr-pal --version`。
- 在临时目录中模拟安装流程，不能覆盖当前系统安装和真实配置。

## 发布顺序

1. 完成 Herdr 合并、测试和四平台构建。
2. 提交并推送 Herdr fork `master`。
3. 完成 herdr-pal 文档或打包修正，执行完整验证并提交。
4. 创建 herdr-pal annotated Tag `v0.6.0`，重新以该 Tag 版本构建 Bundle。
5. 推送 herdr-pal `main` 和 `v0.6.0`。
6. 创建 GitHub Release `v0.6.0`，上传四个 Bundle 和四个 SHA-256 文件。
7. 从 GitHub Release 重新读取资产列表和大小，确认没有草稿、预发布或遗漏资产。

## 失败与回退

- 合并冲突未解决或 Herdr 测试失败时，不推送 Herdr fork。
- 任一平台构建或 Bundle 校验失败时，不创建 Tag 和 Release。
- Tag 推送前发现问题，修复后重新验证；不得移动已经推送的 Tag。
- Release 上传失败时保留 Tag，修复资产后重试同一 Release，不创建内容不一致的新 Tag。
- 不使用 force push，不删除已有 Tag 或 Release。

## 完成标准

- Herdr fork `master` 包含官方 `v0.8.0` 和全部仍需要的 fork 定制。
- Herdr API protocol 19 可被 herdr-pal 建立连接并读取快照、订阅事件。
- 四个平台 Bundle 全部通过架构、版本、校验和与安装模拟验证。
- herdr-pal `v0.6.0` Tag、远端 `main` 和 GitHub Release 指向同一提交。
- Release 只包含约定的八个资产，不包含 Secret、Key、配置文件或临时构建内容。
