# dsh-herdr-pal

Herdr Pal 的第三方 DeepSeek Harness 插件。通过本机 HTTP 接口将 dsh 会话接入现有企业微信
机器人，复用 `/ls`、`/N`、`/N 内容`、普通消息、`/con` 和文本分页。

已针对 **dsh 0.2.1-rc.5 的 web profile** 验证。dsh 接口仍在快速变化，插件用
`engines.dsh` 固定已检查版本。Node.js 需要 22 或更高版本。

## 安装

在 Herdr Pal 仓库构建：

```sh
./unittest.sh
./build.sh
dsh plugin --profile web add /绝对路径/herdr-pal/dist/dsh-herdr-pal-0.1.2.tgz
```

包自带 `cordis.patch.yml`，安装时加入 profile 的 bundle 列表。插件 ID 为
`herdr-pal-bridge`。现有 dsh 进程需要重启以加载插件。

### 内网与离线导入

dsh 官方支持本地 `.tgz`，无需发布到 npm。当前包已经包含可执行 JavaScript，没有
`dependencies` 或安装时构建脚本。其 `peerDependencies` 由完整安装的 dsh 宿主提供。

内网机器需提前安装 Node.js 22+、完整 dsh `0.2.1-rc.5` 和 pnpm（已验证 `10.34.9`）。
本插件 `.tgz` 不包含这三个运行/安装工具；dsh 自身的 npm 单包也不等同于完整运行环境。
复制插件包到内网后执行：

```sh
dsh plugin --profile web add ./dsh-herdr-pal-0.1.2.tgz --offline --ignore-scripts
```

随后按下文配置 token 和 Pal，重启 dsh。内网使用已经编译的 Pal 二进制时无需 Go 工具链。
只有安装插件时需要 pnpm，插件本身不调用 npm、pnpm 或外网接口。

dsh 默认生成的 `pnpm-workspace.yaml` 包含 `nodeLinker: hoisted` 和
`autoInstallPeers: false`，应保留这两个设置。pnpm 可能提示 dsh peer 包未安装在 profile
中；它们实际由宿主加载器提供，不能据此认定插件无法启动。不要为插件再安装另一套
dsh 核心依赖。若同一个 profile 还有其他第三方插件，它们的未缓存依赖也必须提前准备。

已在全新 profile、空 pnpm store/cache、操作系统禁止进程出站联网的条件下，验证本地
tarball 安装和 dsh 启动后的认证目录读取。不访问模型 API 或企业微信。

运行时网络仍按项目架构划分：插件只监听本机回环，Pal 连接配置的 Relay Server；
企业微信 Server 需要能访问企微服务，dsh 执行任务还需要能访问所配置的模型服务。
插件可以离线安装，不代表模型和企微可以完全断网运行。

离线交付前可生成校验文件，复制后再核验：

```sh
shasum -a 256 dsh-herdr-pal-0.1.2.tgz > dsh-herdr-pal-0.1.2.tgz.sha256
shasum -a 256 -c dsh-herdr-pal-0.1.2.tgz.sha256
```

安装机制见 [dsh 官方打包与安装文档](https://deepseek-harness.github.io/deepseek-harness/en/develop/basic/publish)。

生成一个专用于本机连接的 token，保存到安全位置，不要写入仓库：

```sh
openssl rand -hex 32
```

将同一个值分别提供给 dsh 和 Pal。启动 dsh 时设置：

```sh
export DSH_PAL_TOKEN='上一步生成的 token'
dsh web
```

也可把 token 保存到当前用户的普通文件（权限 `0600`，不使用符号链接），并在 profile 的
`cordis.patch.yml` 中指定路径，之后直接运行 `dsh web` 即可：

```yaml
- id: herdr-pal-bridge
  config:
    tokenFile: /绝对路径/dsh.token
```

`DSH_PAL_TOKEN` 优先于文件。Pal 的 `dsh.token` 仍填写同一个值。

插件固定监听 `127.0.0.1:17842`，拒绝无 token 和浏览器 Origin 请求。未提供合法 token
时插件拒绝启动。它不连接企微、不持有机器 Key，不改变 dsh 审批策略。

在 Pal 的配置文件中加入 `dsh`：

```json
{
  "relay": {
    "url": "wss://你的服务端:9443",
    "key": "现有机器 Key",
    "skip_verify": true
  },
  "herdr": { "disabled": true },
  "dsh": {
    "url": "http://127.0.0.1:17842",
    "token": "与 DSH_PAL_TOKEN 相同的值"
  },
  "log": { "level": "info" }
}
```

将配置权限设为 `0600`，使用原有方式启动 `herdr-pal -config /绝对路径/client.json`。
`herdr.disabled=true` 表示只连接 dsh，无需安装或启动 Herdr。要同时列出 Herdr 与 dsh，
删除该字段并保留原来的 Herdr Socket 配置；同一机器仍只建立一条 HPRP 连接。
`skip_verify` 沿用项目原有内网配置；有可信 TLS 证书时设为 `false`。

在企业微信中：

```text
/ls
/3
继续完成刚才的任务
/con
```

`/ls` 中 dsh 显示为 `DeepSeek Harness`。`/3` 的数字来自当前列表；也可以使用
`/3 消息`。任务结束后的文本回复沿用已有状态通知与活跃会话机制。新版本 Pal 兼容
现有 HPRP/1 Server，无需给机器人增加一套命令。

## 会话范围与行为

- 列出当前已加载的根会话，以及最近 **14 × 24 小时**内的历史会话。时间采用 dsh 自身
  列表的 `updatedAt`（创建时间与最近用户输入时间的较大值）；包含时间边界。
- 列表和 `/con` 都不恢复历史 Agent。首次发送才通过公开 `sessionController.resolveAgent`
  恢复，然后走 `Agent.followup`；不会创建一个替代的新聊天。
- 历史会话仍需 dsh 所需的工作目录、模型配置与凭据可用。其他进程持有写锁、需要格式迁移
  或恢复失败时拒绝输入，不抢占会话。需要迁移的历史不列出。
- 子 Agent 不作为独立企微目标。最多返回 256 项，优先已加载会话，再按更新时间取历史；
  Herdr 与 dsh 合并后也受 HPRP 的 256 项上限限制，Herdr 优先。
- 插件重启或 Agent 实例替换使原选择失效，需要重新 `/ls`。历史恢复为 live 属于同一目标，
  不要求重新选择。
- 仅空闲时提交普通消息。工作中、等待审批时请使用 dsh 界面；`/key`、`/enter` 不会变成审批，
  也不会作为 prompt 发送。`/slash TEXT` 沿用普通消息语义。
- 提供最近最多 100 条聊天正文：只保留真实用户输入（`source.kind=user`）和助手的文本块，
  排除工具调用/结果、推理、系统/开发者内容、技能/时间/目标等内部注入和压缩摘要。
  已加载与历史会话都从原始聊天事件提取，不使用压缩后的模型上下文。多轮对话仍保留，
  不是仅限最新一轮或浏览器当前滚动的一屏。
- 最多保留末尾 16000 个 UTF-16 代码单元，分页每页 100 行；图片块暂不展示。dsh 没有可截图
  终端，图片模式返回文本。
- 正常在线时每秒同步一次；断线检测最迟受 5 秒本地请求超时约束，失败后撤下目录。
  网络断线期间不补发通知，输入请求不自动重试；超时请先 `/con` 核对执行情况。
- IM、HPRP、插件各自有去重边界。插件保留 10 分钟、最多 4096 个输入幂等键；容量满时拒绝
  新请求，不提前驱逐有效键。重启后 incarnation 改变，旧请求不能误投新实例。

## 本地协议

所有接口使用 `POST`、`Content-Type: application/json` 和 `Authorization: Bearer TOKEN`：

| 路径 | 请求 | 响应 |
| --- | --- | --- |
| `/v1/sessions` | `{}` | `{protocol:1,sessions:[{id,incarnation,title,workspace,status,revision}]}` |
| `/v1/read` | `{id,incarnation}` | `{text}` |
| `/v1/prompt` | `{id,incarnation,request_id,text}` | `{accepted:true}` |

错误只有固定 `error` 码，不含 token 或原始异常正文。请求体限制 64 KiB，单条 prompt
限制 16 KiB。输入在恢复前同步占用幂等键，避免并发重复提交；目录和读取不会被慢恢复阻塞。
异步读取、恢复后的会话身份都重新核验；Go 客户端禁用代理和重定向。
`revision` 只是本插件用于发现新文本的摘要，与 Herdr 的 revision 无关。

如需多个 dsh 实例，请为它们配置不同端口和各自的 Pal 机器连接。profile 的
`cordis.patch.yml` 可以覆盖插件配置：

```yaml
- id: herdr-pal-bridge
  config:
    port: 17844
```

对应修改 `dsh.url`。当前单个 Pal 只配置一个 dsh 端点，`herdr-pal -i` 仍只操作 Herdr。

## 验证

`node --test plugins/dsh-herdr-pal/test/*.test.mjs` 验证插件认证、幂等、历史过滤与恢复。
`go test ./internal/integration -run TestDSH -count=1 -v` 启动真实插件 HTTP 服务、生产 Pal、
HPRP Server 和企微 Router；只有 dsh Agent 与企微投递使用 fake，不访问模型或 IM 网络。
全量验证已纳入根目录 `unittest.sh`、`build.sh`，构建会生成 npm 安装包。
