# Herdr Sidecar 一键安装包设计

## 1. 目标

将 Herdr 与 Herdr Pal 组成同版本、同平台的一体化安装包。用户解压后执行
`./install.sh`，通过交互输入 HPRP Server URL 和机器 Key，即可完成二进制安装、客户端配置
和 Herdr Sidecar 配置。

首版支持以下平台：

- Linux amd64
- Linux arm64
- macOS amd64
- macOS arm64

不安装系统服务，不修改 Herdr 的 Sidecar 实现，不支持 Windows。

## 2. 产物

每个平台生成一个独立压缩包：

```text
herdr-bundle-<version>-linux-amd64.tar.gz
herdr-bundle-<version>-linux-arm64.tar.gz
herdr-bundle-<version>-darwin-amd64.tar.gz
herdr-bundle-<version>-darwin-arm64.tar.gz
```

压缩包展开后至少包含：

```text
herdr-bundle-<version>-<os>-<arch>/
├── herdr
├── herdr-pal
├── install.sh
└── README.md
```

`herdr` 和 `herdr-pal` 必须安装为同一目录下的真实可执行文件。Herdr 的 Sidecar 解析会优先
寻找自身相邻的 `herdr-pal`，因此不依赖运行时 `PATH` 也能启动正确版本。

## 3. 构建与版本

Herdr Pal 仓库负责一体化包的组装：

- Herdr 源码目录默认为 `~/Code/herdr`，同时允许构建参数覆盖。
- Herdr 使用其现有 Rust release 构建方式；Herdr Pal 使用现有 Go 交叉编译方式。
- 打包脚本拒绝混入目标平台不一致或缺失的二进制。
- 包版本由显式参数或当前 Herdr Pal Git tag 提供，不能静默使用不明确的版本。
- 生成压缩包的同时生成 SHA-256 校验文件。

现有 `build.sh` 继续负责项目常规构建，并增加调用一体化打包的稳定入口。打包中可复用已有
构建结果，但必须先校验目标平台和架构。

## 4. 安装流程

`install.sh` 使用 POSIX shell，流程如下：

1. 检测 `uname -s` 和 `uname -m`，确认当前平台与包一致。
2. 询问安装目录，默认 `~/.local/bin`；支持用户输入其他用户可写目录。
3. 询问 HPRP Server URL，要求使用 `wss://`。
4. 询问机器 Key，输入过程不回显，并确保终止或异常时恢复终端状态。
5. 创建安装目录，将两个二进制安装为 `0755` 权限的真实文件。
6. 若目标文件已存在，先在同目录创建带时间戳的备份，再原子替换。
7. 备份并合并客户端与 Herdr 配置。
8. 校验二进制、客户端配置和 Herdr 配置。
9. 若安装目录不在 `PATH` 中，打印适用于当前 shell 的配置提示。
10. 若检测到 Herdr 正在运行，询问是否执行 `live-handoff`，默认选择“是”。失败时不终止
    当前 Herdr 进程，并给出手工重启提示。

安装器可以重复执行。重复安装不能产生重复 Sidecar 配置，也不能丢失用户的其他配置。

## 5. 配置合并

### 5.1 Herdr Pal

默认文件为 `~/.config/herdr-pal/config.json`，权限设置为 `0600`。已有文件先备份，只更新：

```json
{
  "relay": {
    "url": "wss://server.example.com/hprp",
    "key": "hpk_..."
  }
}
```

`relay` 中其他字段以及顶层 `herdr`、`log` 等字段全部保留。新文件使用当前客户端的安全默认
配置。配置错误必须指出文件路径和具体原因，但不得回显完整 Key。

### 5.2 Herdr

默认文件为 `~/.config/herdr/config.toml`。安装器只添加或更新其管理的 Sidecar 配置：

```toml
[[sidecar]]
command = ["herdr-pal"]
```

已有其他 Sidecar 和 Herdr 配置均保留。安装器使用明确的管理标记识别自己写入的配置；遇到
无法安全合并的 TOML 时停止修改、保留备份并给出人工处理说明。

结构化 JSON/TOML 合并不依赖 `jq`、Python 或其他系统工具。`install.sh` 调用随包提供的
Herdr Pal 配置辅助入口完成解析、验证和原子写入，以保证 Linux 与 macOS 的一致行为。

## 6. Sidecar 运行时对接

Herdr 启动 Sidecar 时会注入：

- `HERDR_SOCKET_PATH`
- `HERDR_BIN_PATH`
- `HERDR_ENV=1`

Herdr Pal 的网络模式在用户未显式配置 `herdr.socket_path` 时，优先使用
`HERDR_SOCKET_PATH`，然后才执行现有 CLI 和默认路径探测。显式配置始终优先，避免改变非
Sidecar 部署的行为。

Herdr Pal 已通过进程上下文处理终止信号；Herdr 退出后 Sidecar 会随其生命周期结束，不再
额外安装守护进程。

## 7. `/help` 更新

更新服务端首次生成 `~/.config/herdr-pal-server/help.md` 时使用的默认内容，并同步调整仓库内
相关示例和测试。安装与配置部分替换为一体化安装包流程：

1. 从项目 Release 下载与操作系统、架构匹配的 `herdr-bundle`。
2. 校验 SHA-256，解压并运行 `./install.sh`。
3. 从企业微信执行 `/userid`，由管理员签发机器 Key。
4. 在安装器中输入 Server URL 和 Key。
5. 启动或切换 Herdr，Sidecar 自动运行 Herdr Pal。
6. 回到企业微信执行 `/ls` 验证接入。

帮助内容不再要求用户分别下载 Herdr 和 Herdr Pal，也不要求手工编辑 JSON/TOML。保留必要
的故障排查入口，并说明高级用户仍可修改生成后的配置文件。

## 8. 安全与失败恢复

- Key 不在终端回显，不写入命令行参数、日志或临时明文文件。
- 配置目录和文件分别使用不宽于 `0700`、`0600` 的权限。
- 所有配置写入采用同目录临时文件加原子替换。
- 二进制替换前保留备份；任一步失败时清理临时文件并报告已完成和未完成的步骤。
- 不自动使用 `sudo`。安装目录不可写时要求用户重新选择，不能自行提升权限。
- `live-handoff` 只在用户确认后执行，失败不能杀死现有 Herdr。

## 9. 测试与验收

单元测试至少覆盖：

- Sidecar Socket 环境变量、显式配置和现有自动探测之间的优先级。
- 新建及合并 Herdr Pal JSON 配置，保证未知合法字段和其他配置不丢失。
- 新建、重复安装及更新 Herdr Sidecar TOML，保证不产生重复配置。
- 非法 URL、非法 Key、损坏 JSON/TOML、只读目录和原子写入失败。
- `install.sh` 的平台识别、默认值、用户自定义安装目录和非交互测试输入。
- 默认 `/help` 包含一体化安装步骤，且删除旧的分别安装说明。
- 四个平台包的文件布局、可执行权限、二进制目标平台和校验文件。

提交前运行根目录 `./unittest.sh` 和 `./build.sh`。真实验收至少在一台 macOS 和一台 Linux
机器上完成安装、重复安装、Herdr `live-handoff`、Sidecar 启动以及企业微信 `/ls` 验证。
