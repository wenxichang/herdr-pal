# hp-cli Cobra 帮助系统设计

## 1. 目标

把 `hp-cli` 现有手写参数解析替换为 Cobra 命令树，使管理员可以从顶层帮助快速发现全部
管理命令，并在任意子命令层级查看准确的用法、参数约束和示例。

本次只调整 CLI 参数与帮助层，不修改 HPAP/1、Admin Socket、服务端方法或权限边界。

## 2. 命令树

根命令包含以下完整层级：

```text
hp-cli
├── server
│   ├── status
│   ├── stop
│   └── debug
│       ├── enable
│       └── disable
├── key
│   ├── issue
│   ├── list
│   ├── show
│   ├── enable
│   ├── disable
│   ├── delete
│   └── source
│       ├── list
│       ├── add
│       ├── remove
│       └── set
├── connection
│   ├── list
│   ├── show
│   └── disconnect
└── session
    └── list
```

每个叶子命令继续生成现有强类型 `Invocation`，复用 `executeInvocation` 和 HPAP 客户端，
不把协议调用逻辑写入 Cobra 命令定义。

## 3. 帮助行为

以下形式均返回退出码 `0`，且不读取配置、不连接 Admin Socket：

```text
hp-cli --help
hp-cli -h
hp-cli help
hp-cli key --help
hp-cli help key
hp-cli key source add --help
hp-cli help key source add
```

根帮助显示：

- 产品用途和基本调用格式。
- `-config`、`--json`、`--version` 等全局参数。
- 全部可执行命令路径及一行摘要，而不只显示第一级命令。
- 常见管理流程示例。

中间命令帮助显示直接下一级命令、用途和该模块的常见示例。叶子命令帮助显示完整用法、
位置参数、可选参数、必填条件、是否允许重复以及格式约束。例如 `key issue --help` 必须
明确 `--principal-id`、`--machine-id` 和至少一个 `--source` 为必填，`--source` 可以重复，
`--expires-at` 使用 RFC3339。

未知命令或无效参数返回退出码 `2`。错误信息后显示当前最接近命令层级的帮助，避免用户
只能退回顶层查找。帮助始终使用人工可读文本，`--json` 不改变帮助格式。

## 4. 参数兼容性

Cobra 根命令提供持久参数 `--config` 和 `--json`，并启用内置 `--help`、`-h` 和
`--version`。参数可以位于子命令前后。

为避免破坏已有脚本，在交给 Cobra 前只规范化以下历史写法：

- `-config PATH` 转换为 `--config PATH`。
- `-config=PATH` 转换为 `--config=PATH`。
- `-version` 转换为 `--version`。

其余未知单横线参数不做宽松转换，由 Cobra 正常拒绝。现有命令名称、位置参数、
`--json` 输出格式和 `-config` 默认路径语义保持不变。

## 5. 错误与退出码

命令执行仍使用现有分类：

- HPAP 服务端业务错误返回 `1`。
- Cobra 参数错误和本地配置错误返回 `2`。
- Admin Socket、HPAP 协议和输出错误返回 `3`。
- 帮助和版本返回 `0`。

Cobra 设置 `SilenceErrors` 和 `SilenceUsage`，由统一运行入口决定何时输出错误及当前层级
帮助，避免重复打印或在服务端业务错误后附加无关用法。

## 6. 代码边界

- `newRootCommand` 只装配命令树、帮助模板、全局参数和输出 writer。
- 各模块构造函数负责自己的 Cobra 子命令和参数校验。
- 公共执行函数把 `Invocation` 交给现有 executor，并负责结果格式化和退出码映射。
- 帮助元数据与命令定义放在同一处，避免维护第二份独立命令清单。
- 根帮助通过遍历 Cobra 命令树生成完整命令路径摘要；子命令帮助使用同一棵树生成直接
  下级列表。

## 7. 测试

单元测试至少覆盖：

- 顶层帮助包含全部叶子命令路径和全局参数。
- `help`、`--help`、`-h` 在根、中间和叶子层级行为一致。
- 叶子帮助包含完整参数约束和示例。
- 所有帮助请求不会调用 executor。
- 历史 `-config`、`-config=...` 和 `-version` 继续可用。
- 原有全部命令仍生成等价的 `Invocation`。
- 参数错误、业务错误、配置错误和传输错误保持原退出码。
- 真实构建出的 `hp-cli` 可以执行顶层和深层帮助，且不依赖运行中的 Server。

实现过程使用测试驱动：先增加失败的帮助与兼容性测试，再替换手写解析代码，最后执行
`./unittest.sh` 和 `./build.sh`。
