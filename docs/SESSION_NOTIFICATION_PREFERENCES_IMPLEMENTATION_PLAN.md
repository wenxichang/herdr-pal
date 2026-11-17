# 会话主动通知开关实施计划

> 执行要求：使用 `superpowers:executing-plans`，由主代理逐项实施和审核；遵守项目要求，不使用 subagent-driven development。使用下方复选框跟踪进度。实施状态见文末执行记录。

**目标：** 在 Server 中提供默认开启、可持久化、可批量设置的会话通知开关；关闭后不发送该组合的任何会话主动通知，用户主动获取仍正常响应。

**架构：** Server 使用已有快照中的业务元数据生成通知身份，独立存储偏好；企微编号只用于找到用户最近看到的业务组合，无效编号逐项跳过。`/ls` 对在线且已同步的机器核对偏好、补充默认开启记录，仅在该机器当前列表非空时清理失效组合。Router 在主动通知读取终端或发送任何内容之前检查偏好，命令响应路径继续正常工作。

**技术栈：** Go 1.26.5、已有 Server Router / SessionCatalog / UserExecutor、JSON 文件原子替换、现有 fake Relay 和企微测试设施。无需新增依赖。

**设计依据：** 本文件“确定的功能语义”是此次实施的完整设计。用户已确认同组合共享、改名默认开启、逗号与空格分隔、无效编号不影响其他有效项，以及 `/ls` 仅清理在线、已同步且当前列表非空的机器中的失效组合；离线、未同步或空列表保留设置。本文取代此前按 Target 持久化、无效编号整批拒绝和永久保留失效组合的建议。

## 全局约束

- 仅修改 Server 实现、相关测试和项目文档，不修改 Pal、Herdr、dsh 后端和 HPRP 消息或 Schema。
- 对话、文档、注释使用中文；变量、函数、类型、JSON 字段使用英文。
- 新增对外接口必须有 Go 文档注释，新增逻辑必须有可在构建环境运行的单元测试。
- 复用现有 `build.sh`、`unittest.sh`，提交前必须完整构建并通过全部单元测试。
- 不绕过消息幂等、身份隔离、用户限速或现有按用户串行队列。
- 用户已于 2026-11-17 明确授权审核后提交并合入 main；本轮仅本地提交和合并，不自动推送远端。

## 源码审核结论

审核基线为 `7b8020c`。任务开始时工作区干净。

| 代码位置 | 已核实行为 | 对设计的影响 |
| --- | --- | --- |
| `internal/server/router.go:119` `sendNotification` | 先更新活跃窗口；可能直接发送失效、状态或简短提醒，也可能读取终端 | 开关必须覆盖所有分支，并移到活跃窗口更新之前 |
| `internal/server/router.go:1320` `statusNeedsTerminal` | done、blocked 和特定 idle 变化读取终端；working、unknown 只发状态 | 过滤整个事件，不能仅跳过 done 正文 |
| `internal/server/router.go:666` `handleSelect` | `/N` 内部执行 `/con` 并回复内容 | 不能对通用发送或终端读取接口加开关 |
| `internal/server/router.go:100` `sendCommandOutput` | 命令后续分段有独立入口 | 后续分段也属于用户操作的响应，必须保留 |
| `internal/relayclient/client.go:323,768` | 状态只上报 `notification.event`；命令结束后单独发送 `command.result` / `command.output` | 当前 done 输出没有通过命令输出绕过通知入口，无需修改 Pal |
| `internal/server/catalog.go:232,256` | `/N` 编号可由终端输出自动重建，单次解析只锁定一个编号 | `/notify` 保存完整送达的 `/ls` 的编号到业务组合映射，独立于后台自动编号；逐项匹配当前组合 |
| `internal/server/catalog.go:196` 和 `internal/server/router.go:635` 附近的目录与列表逻辑 | 在线目录可为空，已同步机器和未同步机器不能仅靠会话列表区分 | `/ls` 需要一次性取得已同步机器范围与会话列表，并允许无会话时进入核对流程 |
| `internal/server/hub.go:570` 附近 | 失效事件允许引用已不在在线目录中的目标 | 保留短期退出来源；组合已由 `/ls` 清理后，迟到失效事件必须忽略，不能按缺省开启漏发 |
| `internal/dsh/executor.go:499` 附近 | 工作区是 cwd 的 basename；Tab 为 web/Desktop；Agent 为 dsh；标签可能截断 | 元数据组合不保证物理唯一，按用户确认规则让相同组合共享设置 |
| `internal/server/catalog.go:590` | 显示模式会跟随同 slot 的会话替换迁移 | 通知规则独立实现，不能直接复制显示模式的迁移逻辑 |
| `internal/serverapp/runtime_files.go` | 已存在的自定义 help.md 不会被默认帮助覆盖 | 更新内嵌帮助，同时在升级说明中提示管理员手动补充已有 help.md |

`done` 当前请求的 `max_lines` 是 **100**，表示近期终端快照，不是全部历史输出。关闭时应同时禁止状态提示、简短提醒、这次快照请求、图片及降级文本。

审核期间执行 `go test ./internal/server ./internal/serverapp ./internal/integration`，三个包均通过。这是现有实现的基线验证；新功能的验证记录见文末。

## 确定的功能语义

### 1. 通知身份和继承

业务身份为六个独立字段组成的结构：

```text
principal_id + machine_id + title + workspace + tab + cli_type
```

- `principal_id` 来自已认证企微单聊身份，`machine_id` 来自连接绑定身份。
- `title`、`workspace`、`tab` 分别取 `Session.Display.Title/Workspace/Tab`，去除首尾空白后按原值匹配，保留大小写及内部空白。
- `cli_type` 优先取 `Display.Agent`，缺失时取 `Display.DisplayAgent`；去除首尾空白并转为小写，均缺失时使用 `unknown`。dsh 的值是 `dsh`，Server 无法从当前协议获知其内部 Agent 的真实 CLI。
- 不使用 `/ls` 序号、Display.Index、slot_id、session_id、格式化后的展示字符串作为持久化键。编码使用结构体字段，禁止用可碰撞的字符串分隔符拼接。
- 允许空标题或空工作区；界面的“未命名”等占位文本不参与身份计算。
- 同组合的所有会话共享设置，即使当前只有其中一个编号被操作；不同用户、机器、Tab 或 CLI 类型互相隔离。`/ls` 明确标注当前在线的同组合条目共享开关。
- 在线组合无记录时默认开启；持久化记录包含身份及显式 `enabled` 状态。`/ls` 为新增组合补充开启记录，开启命令将 `enabled` 设置为 true，不删除记录；重复设置是幂等操作。
- 记录未被 `/ls` 清理时，相同组合重连、换 Target、dsh 宿主重启后沿用设置；新 session_id 不再必然恢复开启。后端暂时报空时执行 `/ls` 不清理该机器原有记录，恢复相同组合仍沿用开关。
- 改名、改工作区或 CLI 类型变化后按新组合匹配，无记录默认开启；如果新组合仍有已保存记录，则沿用那个组合的设置。不自动迁移旧组合；旧组合在 `/ls` 核对后已删除时，改回旧名称也是默认开启，未删除时沿用已有设置。
- `/ls` 的具体增删规则见下一节。配置文件不保存终端内容或凭据，权限保持 `0600`。

### 2. `/ls` 核对与失效清理

- 在 catalog 的一次锁内捕获当前用户已在线且完成首个完整快照同步的机器 ID，以及它们的当前会话列表；两者必须来自同一视图。已同步机器即使当前零会话，也属于核对范围；未同步或离线机器不属于核对范围。
- 按业务组合去重后与保存记录对账：已有组合保留其开关；新组合添加 `enabled=true`；仅在该机器本次列表非空时删除其中已不再出现的组合。组合仍有任意一个在线会话匹配就不能删除。
- 清理只针对当前用户、本次已同步在线且列表非空的机器；条件按每台机器分别判断，其他机器有会话不能使空列表机器进入清理范围。保留其他用户、离线、未同步及空列表机器的全部记录。仅因连接断开或 Server 刚重启、目录尚未恢复，不删除开关。
- 在线机器上报空快照时保留该机器全部设置；真正零会话时也不立即清理，等后续列表非空再删除其中缺失的旧组合。全部机器离线或上报空列表时，`/ls` 返回无在线会话，原有记录保持不变。
- 核对以本次目录快照为准，不为每个组合发远端请求。非空条件避免整台机器暂时报空导致设置丢失；列表非空但部分会话暂缺时，仍按当前非空目录清理，Server 不额外推断每个后端的健康状态。
- 一次事务完成新增、保留和删除，并返回该视图各组合的实际状态供列表展示。先核对并成功保存，再显示 `通知：✔️ 开启 / ✖️ 关闭`；保存失败时不提交任何核对变更，返回“通知设置核对失败，请重试”，不宣称清理成功，也不启用新的 notify 编号映射。
- `/ls` 负责对账，不承担通知默认值初始化的前置条件；未执行 `/ls` 的在线新组合仍按开启处理。`/notify` 也可直接为当前在线组合创建或更新记录。
- 列表发送失败不回滚已提交的对账结果，但不能启用未完整送达的编号映射。普通 `/N`、主动查询及通用编号语义不改动。

### 3. 命令

| 命令 | 语义 |
| --- | --- |
| `/notify` | 查看当前选中会话所属组合的开关 |
| `/notify on`、`/notify off` | 设置当前选中会话所属组合 |
| `/notify on 1 2 3`、`/notify off 1,2,3` | 根据最近完整送达的 `/ls` 逐项找到业务组合，设置仍存在的组合，跳过无效项 |
| `/notify off 1, 2 3` | 逗号和空格可混用，中文逗号也支持 |
| `/notify on all`、`/notify off all` | 设置执行时当前用户全部在线会话涉及的组合，无需先执行 `/ls` |

规则：

- 接受小写关键字，编号按空白、英文逗号或中文逗号分隔，连续分隔符忽略。有显式分隔符但没有任何编号时提示用法，不误当“设置当前会话”。
- 无效状态、`all` 与编号混用、分号拼接多条命令属于命令整体语法错误，不执行；非正 ASCII 编号、整数溢出或非数字项作为逐项无效输入跳过，不影响其余有效编号。
- 重复编号去重，多个编号命中同组合也去重；无须套用注册审批的 10 项限制，仍受企微现有单条消息长度限制。
- `/N /notify ...` 和 `#N /notify ...` 明确拒绝，避免与 notify 自带的批量编号形成两层目标范围；不会转发到 Pal。
- 无显式目标时复用 `Selected`：仅一个在线会话可自动选中，多会话无选择则提示先 `/ls` 和 `/N`。显式编号和 `all` 不改变当前选择。
- 编号越界、对应组合已不存在或所属机器离线时，逐项跳过，继续处理其余存在的组合。只要相同业务组合仍存在，即使原 Target 已替换，也可继续设置。列表中旧标题已不存在时不改到新标题。
- 最近完整送达的 `/ls` 映射缺失时，无法判断任何显式编号的原意，回复先执行 `/ls`；全部输入都被跳过时不写文件，列出原因。去重后的有效组合使用一次 Store 操作提交，磁盘失败则这些有效组合都不生效；这是存储事务失败，不是无效编号导致的失败。
- `all` 捕获当前用户在线目录的一份快照，只处理其中的业务组合；未来出现的新组合默认开启。由于同组合共享，稍后出现的同组合会话也会沿用此次设置；离线的其他组合不受影响。
- 成功或部分成功回复显示当前受影响的在线会话数、去重后的组合数和最终状态，并列出跳过的编号/输入及原因，说明同组合共享。例如 `已关闭 2 个组合（当前 3 个会话）；跳过 99：编号不存在。` 存储失败则明确有效项均未生效，不泄漏存储正文或本地敏感数据。
- 无在线会话时明确回复无可设置目标，`all` 不产生记录；解析错误仍返回用法。

### 4. 编号与并发边界

- `/ls` 核对并成功发送全部分段后，提交一份仅供 `/notify` 使用的编号到业务身份映射，不把完整 Target 作为组合仍存在的判断条件。每次新的 `/ls` 尝试先使旧的 notify 映射失效，核对或发送失败都不启用新映射。
- notify 映射只能由成功的 `/ls` 建立；`EnsureNumberedIndex` 的后台自动编号不能覆盖它。带编号的 `/notify` 始终引用最近完整送达的 `/ls`，不因后台输出重编号而整体拒绝。
- 一次读锁内取得当前用户在线组合集合；逐项从已展示映射查业务身份，再匹配当前集合，分别收集有效组合和跳过原因。不要求原 slot/session 仍存在，不用当前编号位置重新解释旧编号。
- 新增在线会话不挪动旧 notify 编号，新组合需要重新 `/ls` 才有显式编号；旧组合出现新 Target 或更多同组合会话无需重新列表。连接断开不整体销毁 notify 映射，仅使该机器当前无法匹配的项被跳过，其他机器的有效项继续生效。
- 匹配时点是本次在线组合集合的捕获时点。之后固定业务身份列表，不再重读编号；随后会话消失或改名不会将设置转移到新组合。
- 目录锁不跨越磁盘或企微网络调用。偏好 Store 用独立锁将候选状态整批保存，文件替换成功后才一次发布内存状态。
- 同一用户的命令和通知继续由 `UserExecutor` 串行处理。已在关闭命令之前开始处理的通知可完成；关闭成功回复之后处理的通知按新设置判断，不取消已发出的消息或中断先前的读取。
- 若设置已落盘但成功回复发送失败，不回滚设置；重发相同设置仍安全，用户可通过 `/ls` 核实。

### 5. 主动通知过滤及退出目标

- 保留 Hub 的协议验证、目标隔离、序号及幂等检查。Router 验证事件类型和机器后，再解析通知来源并检查业务组合开关。
- 状态事件必须仍可解析到当前在线 Target。失效事件可以使用当前元数据，或最近退出目标的元数据；不能强制要求旧 Target 仍在线。
- `SessionCatalog` 在快照移除或替换 Target 时，保留该旧 Target 最后一次确认的 `CatalogEntry`。短期缓存按用户和完整 Target 隔离，TTL 为 10 分钟，总容量为 4096，满时先删除过期项再淘汰最早退出项；注入时钟以便确定性测试。连接 Detach 清除该连接所属机器的退出缓存。
- 退出缓存只用于过滤失效通知，不能用于输入、选择、终端读取、对账或恢复在线目录。未知或已淘汰的失效来源直接忽略并记脱敏 debug 日志。
- `/ls` 成功对账删除组合后，同步移除该组合的退出来源，迟到失效事件找不到来源时忽略。其他可确认的在线或退出来源都按组合的当前开关过滤，无记录默认开启；因此未执行过 `/ls` 的会话保留原有默认通知行为。
- 关闭时直接返回 `nil`，不更新活跃窗口、不调用 `EnsureNumberedIndex`、不发送任何状态/失效/简短提醒、不请求终端快照，也不产生终端投递审计。
- 开启时保留现有状态、快照、图片及文本降级策略。重新开启不主动取快照，不重放关闭期间已消费的事件，等待新的有效事件；用户可随时 `/con`。
- `/ls`、`/N`、`/con`、翻页、定向查询、输入确认及 `command.output` 不检查通知开关。即使底层使用主动发送 API 发送命令后续分段，也仍属于用户操作的响应。
- 注册、审批及过载回复等非 Agent 会话通知保持现有策略。

## 审核重点

1. 同标题跨用户、机器、Tab、CLI 的隔离，以及同组合跨 Target 的继承：任务 1、3、5。
2. 逗号混用、部分编号无效、输出自动重编号、原 Target 已替换但组合仍在：任务 2、3。
3. `/ls` 对账、在线空列表与离线的区别、清理后迟到失效事件：任务 1、2、3、4。
4. `/ls` 部分发送失败、文件写入失败或跨用户并发更新导致内存和磁盘分歧：任务 1、3、5。
5. 关闭后 done 的正文、状态提示或图片降级仍漏发，以及主动查询被误拦截：任务 4、5。

## 文件职责

| 文件 | 责任 |
| --- | --- |
| 新增 `internal/notificationprefs/identity.go`，测试集中于 `store_test.go` | 业务身份的字段、规范化和纯函数测试 |
| 新增 `internal/notificationprefs/store.go`、`store_test.go` | 显式开关、范围对账、事务式持久化、恢复和失败一致性 |
| 新增 `internal/server/catalog_notification.go`、`catalog_notification_test.go` | 已展示编号到组合映射、逐项解析、在线核对视图、退出来源缓存 |
| 修改 `internal/server/catalog.go` | 在编号/快照/连接生命周期调用上述局部逻辑，不引入磁盘读写 |
| 新增 `internal/server/router_notification.go`、`router_notification_test.go` | notify 命令解析、执行、回显、共享状态展示辅助逻辑 |
| 修改 `internal/server/router.go` | 注入偏好、命令分派、`/ls` 状态及发送成功后提交快照、通知过滤 |
| 修改 `internal/server/logging.go` | 新增 notify 动作名及脱敏错误分类 |
| 修改 `internal/serverapp/app.go`、`app_test.go` | 加载持久化偏好并注入生产 Router，启动失败测试 |
| 新增 `internal/integration/notification_preferences_test.go` | 复用 Relay / Server / fake 企微设施做本地端到端验证 |
| 修改 `internal/integration/dsh_native_test.go` | 修复既有多题审批测试仅等待状态、未等待提示送达的时序问题；不改 dsh 实现 |
| 修改 `internal/server/default_help.md`、根目录 `help.md`、`help_test.go`、`README.md`、`docs/HANDOFF_CONTEXT.md`、`docs/BRIDGE_ARCHITECTURE.md` | 命令、持久化和升级帮助说明 |

## 实施任务

### 任务 1：实现业务身份和事务式偏好存储

**接口：**

- `notificationprefs.Identity`：`PrincipalID, MachineID, Title, Workspace, Tab, CLIType string`；结构可比较，用作内存 map 的键。
- `notificationprefs.NewIdentity(principalID, machineID string, display hprp.SessionDisplay) (Identity, error)`：实现上述取值和规范化，校验可信身份字段；不要求 Target ID。
- `notificationprefs.NewMemoryStore() *Store`、`notificationprefs.LoadStore(path string) (*Store, error)`。
- `(*Store).Enabled(identity Identity) bool`：在线组合未记录时默认 true；`(*Store).Lookup(identity Identity) (enabled bool, exists bool)`：额外区分未记录状态，供查询和对账验证使用。
- `(*Store).SetBatch(identities []Identity, enabled bool) error`：只更新传入组合，空列表无副作用，不隐式清理其他记录。
- `(*Store).Reconcile(principalID string, machineIDs []string, current []Identity) (ReconcileResult, error)`：事务式对账。`ReconcileResult` 包含 `Added, Removed int`、`States map[Identity]bool`、`RemovedIdentities []Identity`，States 是本次 current 对应的实际状态副本，RemovedIdentities 供清除已删除组合的退出来源。machineIDs 只可包含同次目录视图中已完成首快照同步的在线机器；current 必须属于指定用户和这些机器。Store 从 current 按机器计算非空条件，只有对应机器至少存在一个组合时才删除缺失记录。

- [x] 在 `store_test.go` 编写 `TestIdentityScopesAndNormalizesBusinessFields`：相同业务元数据产生相同键；首尾空白去除、CLI 小写；用户/机器/title/workspace/tab/CLI 任一不同即区分；空标签和分隔符内容不碰撞。
- [x] 在 `store_test.go` 编写 `TestStoreDefaultsAndBatchRoundTrip`、`TestStoreBatchFailureIsAtomic`、`TestStoreConcurrentUpdatesPreserveAllGroups`、`TestStoreRejectsInvalidFile`，分别断言默认开、显式开/关记录及重新加载、有效组合提交写失败时保持原内存和文件、并发不覆盖、损坏/版本/无效身份/尾随 JSON/过宽权限被拒绝。
- [x] 编写 `TestStoreReconcileAddsPreservesAndPrunesScopedGroups`、`TestStoreReconcilePreservesEmptyAndOfflineMachines`、`TestStoreReconcileFailureIsAtomic`：新组合记录为开启，已有关闭保留，重复组合只一条；指定在线机器 current 为空时保留该机器记录，machineIDs 为空时不清理任何记录；增加 `TestStoreReconcileRequiresNonemptyListForEachMachine` 验证多机器混合视图中仅非空机器可清理；离线机器和其他用户不变；清理后 Lookup.exists=false；写入失败新增和删除都不生效。
- [x] 运行 `go test ./internal/notificationprefs -count=1`，确认新接口缺失导致失败。
- [x] 实现两个模块。磁盘格式固定 `version: 1` 和 `records` 数组，每项为 `identity` 对象及布尔 `enabled`，同时保存开启和关闭状态，稳定排序；不存在文件视为空，禁止损坏后静默重置。SetBatch 和 Reconcile 都使用候选 map、同目录临时文件、`0600`、写入、Sync、Close、Rename、发布内存，借鉴 `machinereg.Store`；Rename 是提交点，之后目录同步失败不得报告“未生效”并保留旧内存。复用该项目已有的目录同步尽力策略。
- [x] 为临时文件写入/同步/替换注入可测试失败点，断言 Rename 之前失败不影响旧状态；无变化的重复设置不必写文件。
- [x] 运行 `go test -race ./internal/notificationprefs -count=1`，预期全部通过。

### 任务 2：补齐组合匹配、在线核对视图和退出来源缓存

**接口：** 在 `catalog_notification.go` 增加：

- `NotificationInventory` 包含 `MachineIDs []string` 和 `Entries []CatalogEntry`；`(*SessionCatalog).NotificationInventory(userID string) NotificationInventory` 一次读锁取得已同步在线机器及它们的会话，空会话的已同步机器不能遗漏。
- `(*SessionCatalog).CommitNotificationList(userID string, entries []CatalogEntry) error`：提交完整送达列表中的编号到业务身份映射；不因期间 Target 变化使其他编号失效，后续执行逐项核对组合。
- `(*SessionCatalog).InvalidateNotificationList(userID string)`。
- `NotificationIndexIssue` 包含 `Input, Reason string`；`NotificationBatchResolution` 包含 `Identities []notificationprefs.Identity`、`AffectedSessions int`、`Skipped []NotificationIndexIssue`。
- `(*SessionCatalog).ResolveNotificationGroups(userID string, indexes []int) (NotificationBatchResolution, error)`：一次读锁按已展示映射查业务身份，再匹配当前在线组合；有效项去重，失败项逐项返回。error 仅表示缺少整个已展示映射或调用依赖错误，不能因一个编号不存在而返回整批错误。
- `(*SessionCatalog).ForgetRetiredNotificationGroups(userID string, keys []notificationprefs.Identity)`：只在对账提交成功后删除相应退出来源。
- `NotificationSource` 包含 `Entry CatalogEntry`、`Retired bool`；`(*SessionCatalog).ResolveNotificationSource(userID string, target hprp.Target, allowRetired bool) (NotificationSource, error)`：在线优先，只有失效事件可使用退出缓存，并标明来源是否已退出。
- `(*SessionCatalog).recordRetiredNotificationSourcesLocked(key ClientKey, previous, next []hprp.Session)`：只记录已退出的完整 Target，使用 catalog 内部可注入时钟。

- `(*SessionCatalog).createNumberedSnapshotFromEntries(userID string, entries []CatalogEntry) []CatalogEntry`：供 Router 用已捕获视图建立原 `/N` 编号并返回副本；不能再次读取目录。保留 `CreateNumberedSnapshot(userID string) []CatalogEntry` 的现有行为，共享内部写入编号辅助逻辑。

- [x] 编写 `TestNotificationInventoryIncludesSyncedEmptyMachines`：同一视图包含已同步空机器和非空机器，排除未同步或离线机器、其他用户；普通目录的在线语义不受影响。
- [x] 编写 `TestNotificationListUsesCapturedInventory`：捕获视图后目录发生变化，列表、对账输入和提交映射仍来自同一批 Entries，不能展示未对账的新组合；普通 `/N` 继续独立校验完整目标。
- [x] 编写 `TestNotificationListRequiresDeliveryCommit`、`TestNotificationBatchSkipsMissingGroups`、`TestNotificationBatchUsesDisplayedIdentityAfterTargetReplacement`、`TestNotificationBatchIgnoresAutomaticRenumbering`：无成功 `/ls` 提示先列表；混合有效/越界/离线/已改名项时仅跳过无效项；原 Target 消失但同组合仍在时有效；后台重编号不覆盖已展示映射；只有另一个不同标题的会话占用旧编号时不能误改它。
- [x] 编写 `TestNotificationRetiredSourceIsolationAndBounds`：移除/替换后可查旧元数据；普通 ResolveTarget 仍拒绝旧目标；10 分钟到期、4096 容量淘汰和 Detach 清理可预测；跨用户和机器不可查到旧条目。
- [x] 运行 `go test ./internal/server -run 'TestNotification(Inventory|List|Batch|Retired)' -count=1`，确认新行为失败。
- [x] 实现局部模块，在现有 catalog 的 snapshot 和 Detach 生命周期接入来源缓存；notify 映射独立保留，避免一个机器离线让其他有效项失效。不改变原 `/N`、选择、显示模式或快照序号语义。
- [x] 运行 `go test -race ./internal/server -count=1`，预期新增测试和原目录/路由测试均通过。

### 任务 3：实现 notify 命令和 `/ls` 状态展示

**接口：**

- `NotificationPreferences` 接口：`Enabled(notificationprefs.Identity) bool`、`Lookup(notificationprefs.Identity) (bool, bool)`、`SetBatch([]notificationprefs.Identity, bool) error`、`Reconcile(string, []string, []notificationprefs.Identity) (notificationprefs.ReconcileResult, error)`。
- `ConversationRouterConfig.NotificationPreferences NotificationPreferences`；nil 时只为测试或内嵌调用创建独立 MemoryStore，生产入口必须显式注入持久 Store。
- `notificationAction`：`hasSetting, enabled, all, hasExplicitIndexes bool`、`indexes []int`、`issues []NotificationIndexIssue`；显式编号全部无效时不得回退设置当前选择。`serverAction` 增加 `notification *notificationAction` 和 `serverActionNotify`。
- `parseNotificationAction(fields []string) (notificationAction, error)`、`(*ConversationRouter).handleNotify(ctx context.Context, message im.IncomingText, action notificationAction)`。

- [x] 编写 `TestParseNotificationAction` 表驱动用例覆盖 `1,2,3`、`1, 2 3`、中文逗号、连续分隔符、去重、空编号、部分非数字/非正/溢出项、all 混用、分号及定向嵌套；断言无效编号有逐项原因、整体语法错误不执行且不误当普通 prompt，显式编号全部无效不操作当前选择。
- [x] 编写 `TestRouterNotificationBatchAndAll`、`TestRouterNotificationSharedGroupAndRename`：`1,99,2` 只跳过 99，有效组合一次 SetBatch，相同组合只设置一次；相同组合换 Target 仍成功；全部跳过不写入，存储失败有效项均不生效；跨用户隔离、all 不改其他离线组合或当前选择；回复列出成功数量和跳过项。
- [x] 编写 `TestRouterListReconcilesNotificationPreferences`：列表前完成对账，新组合开启、已有关闭保留、重复组合只一条记录、在线范围内失效组合删除、其他用户及离线机器保留；改名后新组合开启，旧组合最后一个会话消失才删除，清理后改回旧标题也默认开启。
- [x] 编写 `TestRouterListPreservesSyncedEmptyMachine`、`TestRouterListReconciliationFailureIsReported`：已同步零会话仍能进入 `/ls`，但保留该机器记录，所有机器离线也不删除；保存失败明确回复失败、不发布部分状态或 notify 映射。
- [x] 编写 `TestRouterListShowsNotificationPreferences`、`TestRouterListCommitsNotificationSnapshotOnlyAfterFullDelivery`：对账返回的实际状态逐项显示 `通知：✔️ 开启 / ✖️ 关闭`，同组合重名显示共享；首段或后续分段失败不能启用映射，但不回滚已成功的对账；完整送达后可以逐项匹配。
- [x] 运行 `go test ./internal/server -run 'Test(ParseNotification|RouterNotification|RouterList)' -count=1`，确认新增行为失败。
- [x] 实现命令模块，并在 Router 接入。`/ls` 先使旧 notify 映射失效，捕获 NotificationInventory 并 Reconcile，通过 `ForgetRetiredNotificationGroups` 清理已删除组合的退出来源，再通过任务 2 的 `createNumberedSnapshotFromEntries` 用同一视图建立现有 `/N` 编号和列表；不能二次捕获不同目录生成内容。检查 `reply` 返回值，全部成功再提交 notify 映射；handleAuthorized 允许 `/ls` 在无会话时到达核对流程，notify 解析错误及无在线目标回复也须可达。
- [x] 更新 `parseDirectedAction` 的拒绝文案及 `serverActionName` 返回值 `notification_preferences`。复用输入幂等和限速；批量审计不可把当前选中会话误记为整批唯一来源，成功/失败日志仅记录数量、状态和身份摘要。
- [x] 运行 `go test -race ./internal/server -count=1`，预期全部通过。

### 任务 4：拦截全部会话主动事件并保留主动查询

**接口依赖：** 任务 1 的身份/Store、任务 2 的来源解析、任务 3 的 Router 注入；不新增 RelayRequester 或 Pal 接口。

- [x] 在 `router_notification_test.go` 编写 `TestRouterMutedNotificationHasNoSideEffects`：枚举 done、blocked、working、unknown、相关 idle，以及图片模式和后台活跃窗口；关闭后发送、图片、FetchTerminalSnapshot 调用增量均为 0，活跃窗口不刷新。
- [x] 编写 `TestRouterMutedInvalidationUsesRetiredIdentity`、`TestRouterPrunedGroupDoesNotEmitLateInvalidation`：关闭组合移除后不发送失效通知；开启的可确认退出来源正常发送，即使从未执行 `/ls`；对账删除后退出来源一并清理，迟到事件忽略；未知/过期来源也忽略；在线新组合无记录仍默认开启。
- [x] 编写 `TestRouterMutedGroupStillAnswersExplicitCommands`：关闭后 `/ls`、`/N`、`/con`、翻页、`#N /con`、命令确认和 `SendCommandOutput` 仍按原路径产生回复，图片失败时主动查询的文本降级仍有效。
- [x] 编写 `TestRouterNotificationToggleQueueOrdering`、`TestRouterReenableDoesNotReplayMutedEvents`：用通道控制队列，不用固定 sleep；关闭前的任务可以结束，关闭成功后处理的任务抑制，重新开启本身不读快照/补发。
- [x] 运行 `go test ./internal/server -run 'TestRouter(Muted|Pruned|NotificationToggle|Reenable)' -count=1`，确认过滤缺失导致失败。
- [x] 在 `sendNotification` 将事件合法性、来源解析和偏好检查放在所有发送、活跃状态更新和快照读取之前；所有可确认来源使用 Enabled 的默认开启语义，实际对账删除的组合已同步清除退出来源。保持命令结果和通用发送接口的行为。
- [x] 运行 `go test -race ./internal/server -count=1`，并确认原 `TestRouterStatusDoneFetchesSnapshotUsingCurrentMode` 和后台通知策略测试继续通过。

### 任务 5：接入生产启动、端到端验证和文档

**接口依赖：** `notificationprefs.LoadStore` 和 `ConversationRouterConfig.NotificationPreferences`。存储路径固定为 `<server.state_dir>/notification_preferences.json`，不新增用户必填配置。

- [x] 在 `internal/serverapp/app_test.go` 增加 `TestRunRejectsInvalidNotificationPreferencesWithoutLeakingContent`，覆盖损坏、错误版本和过宽权限，错误包含文件路径及原因但不含正文；运行对应测试确认失败。
- [x] 在 `serverapp.Run` 监听网络和启动企微连接之前加载持久 Store，错误立即返回，成功后注入 Router；新增文件缺失不影响旧部署启动。
- [x] 新增 `TestNotificationPreferencesEndToEnd`：复用真实 Hub / RelayClient 和本地 fake executor，先 `/ls`、用有效编号与 99 的逗号混合命令批量关闭，确认有效组合关闭且回复跳过 99，再发送 done；用确认已进入并完成 Router 通知处理的测试同步屏障断言零推送、`SnapshotStats` 计数不增加，随后 `/con` 和 `/N` 正常返回。重新开启后新的 done 仅读取一次最近 100 行。
- [x] 新增 `TestNotificationPreferencesSurviveServerReloadAndTargetReplacement`：在临时状态目录保存后重建 Store/Router，重新应用相同组合但不同 Target 的目录，仍关闭；另一个用户/机器/Tab 不继承；改名默认开；关闭组与开启组并存时只推送开启组。测试可扩展 `internal/integration/relay_test.go` 的 helper，禁止修改生产 Pal。
- [x] 新增 `TestNotificationPreferencesListPrunesOnlySyncedOnlineMachines`：使用两台机器，一台离线一台在线，在线机器移除组合后执行 `/ls`，检查磁盘新增和删除范围、离线机器记录保留、清理后迟到失效通知不发送；离线机器重连同组合继续沿用设置。
- [x] 运行 `go test -race ./internal/notificationprefs ./internal/server ./internal/serverapp ./internal/integration -count=1`，预期通过；集成测试不得访问真实 IM 或用户现有 Agent。
- [x] 更新 README、默认帮助及帮助测试：逗号/空格混用、部分成功示例、共享组合规则、改名与 all 范围、`/ls` 对账及离线保留、最近 100 行、关闭不影响主动查询。保留管理员自定义 help.md，记录升级时手动补充新命令的方法。
- [x] 更新 HANDOFF_CONTEXT 和 BRIDGE_ARCHITECTURE：仅通知偏好持久化，选择/在线目录/分页仍为内存状态；不保存或回放通知事件正文；说明退出缓存与稳定命令目标的隔离。
- [x] 执行 `./unittest.sh`，预期格式检查、vet、全部测试及环境支持的竞态检查成功。
- [x] 执行 `./build.sh`，预期现有全部平台构建和校验和生成成功。
- [x] 执行 `git diff --check` 并人工检查变更范围：除测试辅助和文档外仅 Server 及 notificationprefs 包发生变更；确认 `internal/relayclient`、`internal/bridge`、`internal/dsh`、`internal/hprp` 和 Pal 配置无修改。
- [x] 用户已要求提交并合入主线；仅在本轮完整构建和全部单元测试通过后执行，提交信息使用 `feat: 添加会话主动通知开关`。

## 验收场景

1. 初次 `/ls` 每项显示通知开启；`/notify off 1 2 3` 一次设置对应组合，选择保持不变。
2. 静默组合收到 done 时无状态提示、正文、图片或降级文本，且无通知快照读取；`/con` 和 `/N` 可立即获取内容。
3. `/notify off all` 覆盖当前用户在线组合，其他用户和其他离线组合不变；未来新组合仍开启。
4. `/notify off 1,99,2` 及 `/notify on 1, 2 3` 支持混合分隔；99 或其他缺失组合只被跳过，不妨碍有效项；后台自动编号变化不改变用户最后 `/ls` 的组合映射。
5. 两个 Target 拥有完全相同组合时共同开关，换 Target 或宿主重启继续继承；原编号的 Target 消失但同组合仍在时可继续设置，不同新标题不能被旧编号误改。
6. `/ls` 为新组合增加开启记录、保留已有状态、删除已同步在线机器中消失的组合；同组合还有会话时不删，离线机器和其他用户不受影响；在线空快照保留原有设置；仅对应机器列表非空时才清理。
7. pane 退出后的旧目标失效消息遵守记录状态；组合被 `/ls` 清理后不会因默认开启漏发迟到消息。改回已清理的旧标题默认开启。
8. Server 重启保留偏好；有效组合写入失败均不生效，`/ls` 核对写入失败新增/删除均不生效；重新开启不回放关闭期间的事件。

## 计划自审

- [x] 需求覆盖：默认开、Server 独立实现、逗号/空格批量及 all、无效项跳过、`/ls` 对账展示与失效清理、done 完整拦截和主动查询保留均有任务与验收场景。
- [x] 纳入用户最终确认的组合身份及共享/改名规则，删除按 Target 持久化的旧建议。
- [x] 编号只负责本次寻址，持久化与派发均使用同一个业务身份生成接口。
- [x] 明确了部分成功、组合跨 Target 匹配、部分发送、已同步空机器与离线的区别、清理后迟到事件、共享组合、写入失败、重启与队列时序的行为。
- [x] 所有跨任务接口名称和类型一致；未引入 Pal 修改、协议扩展、自动审批、离线通知或无关重构。
- [x] 采用主代理直接实施和独立复核轮次，不使用子代理。


## 执行记录

- 工作分支：`feat/session-notification-preferences`；未自动提交或合并。
- 任务 1—4 已完成 RED→GREEN 单元验证及 Server 竞态检查；生产启动严格加载和本地端到端/重启测试已通过。
- 既有集成测试时序修复：`internal/integration/dsh_native_test.go` 多题审批用例补充等待问题提示实际送达，避免仅观察到 blocked 就提交答案；不改 Pal 或 dsh 实现。
- 实施复核调整：保持原有未执行 `/ls` 也默认通知的行为；通过对账返回已删除组合并同步清理退出来源来防止迟到通知，而不是把所有无显式记录的退出来源都静默。已有默认通知与清理后迟到事件两类回归均通过。
- 文档、默认和发布版帮助已更新；`./unittest.sh` 全部通过，包含普通集成测试及竞态集成测试；最终 `./build.sh` 通过，多平台产物及 SHA256SUMS 已生成。
- Git 提交步骤未执行：用户未要求提交或合并。


## 后续复核

- 全面复核报告见 [会话通知开关方案与代码复核](SESSION_NOTIFICATION_PREFERENCES_REVIEW.md)。
- 上一轮全面复核补充 10 组边界测试，当时未修改生产实现；本次按新增非空条件同步修正实现和相关测试。
- 已按用户新增约束修正：只有对应机器在线、已同步且列表非空时才删除；空列表期间保留关闭记录，恢复相同组合后仍保持关闭。
- 新增本地 HPRP 回归覆盖临时空列表、`/ls`、恢复与 done 全流程；仍保留在线非空目录清理缺失组合的行为。

- 非空条件最终验证：`./unittest.sh`（含全量集成和竞态）及 `./build.sh` 均通过；非空条件按机器分别判断，Pal 和协议生产源码未修改。

- 用户已确认测试机通知功能通过；通知状态统一显示为 `✔️ 开启` / `✖️ 关闭`，已通过全量验证并按既定安排部署 `hp-internal`。

- hp-internal 部署完成：沿用现有 systemd 服务；以服务器 help.md 为基础增补并验证原文字节保留，未上传仓库帮助文件。32 台 Pal 全部重连；见 [部署记录](SESSION_NOTIFICATION_INTERNAL_DEPLOYMENT_2026-11-12.md)。

## 主线收尾（2026-11-17）

用户已明确要求审核、提交并合入主线。已重新审查隔离、部分成功、非空清理、原子持久化、通知过滤和主动查询路径，未发现新的阻断项；已核对远端 main 与当前基线一致。完整验证后在功能分支提交并快进合入本地 main，不改变已部署环境。最终提交号以 Git 历史为准。

- 本轮提交前验证：`./unittest.sh`（全量单测、集成、竞态）、`./build.sh`（全平台构建）及变更格式/凭据检查全部通过，允许提交与快进合并。
