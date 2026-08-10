# Herdr 0.8.0 Bundle 发布实施计划

> **执行方式：** 使用 `superpowers:executing-plans` 在当前会话逐项执行；根据项目约束，不使用 subagent-driven development。

**目标：** 将官方 Herdr `v0.8.0` 安全合入 fork，保留 CSC、Sidecar 和图形流修复，并发布仅含四个平台 Bundle 与校验文件的 herdr-pal `v0.6.0`。

**架构：** Herdr fork 使用普通 merge commit 吸收官方 0.8.0；herdr-pal 仅修正文档和打包元数据，不改变 HPRP 或业务协议。构建阶段从同一份合并后的 Herdr 源码交叉编译四个平台，再由 herdr-pal Bundle 脚本组装并验证。

**技术栈：** Rust/Cargo/Just、Go 1.26、Zig 交叉编译、POSIX Shell、Git/GitHub CLI。

---

### 任务 1：提交发布设计和实施计划

**文件：**

- 新建：`docs/HERDR_0_8_BUNDLE_RELEASE_DESIGN.md`
- 新建：`docs/HERDR_0_8_BUNDLE_RELEASE_PLAN.md`

- [ ] **步骤 1：检查设计和计划没有占位内容或格式错误**

运行：

```bash
rg -n 'T[O]D[O]|T[B]D|待[定]|implement[[:space:]]+later|fill[[:space:]]+in' \
  docs/HERDR_0_8_BUNDLE_RELEASE_DESIGN.md \
  docs/HERDR_0_8_BUNDLE_RELEASE_PLAN.md
git diff --check
```

预期：`rg` 无匹配，`git diff --check` 无输出。

- [ ] **步骤 2：执行 herdr-pal 提交前验证**

运行：

```bash
./unittest.sh
./build.sh
```

预期：两个脚本退出码均为 `0`，Go 单测、竞态检查、vet 和所有目标构建全部通过。

- [ ] **步骤 3：提交文档**

运行：

```bash
git add docs/HERDR_0_8_BUNDLE_RELEASE_DESIGN.md \
  docs/HERDR_0_8_BUNDLE_RELEASE_PLAN.md
git commit -m 'docs: 设计 Herdr 0.8.0 Bundle 发布流程'
```

预期：创建一个只包含两份发布文档的提交。

### 任务 2：合入官方 Herdr v0.8.0

**文件：**

- 修改：`../herdr/docs/next/CHANGELOG.md`
- 修改：`../herdr/docs/next/api/herdr-api.schema.json`
- 修改：`../herdr/docs/next/website/src/content/docs/{agents,integrations}.mdx`
- 修改：`../herdr/docs/next/website/src/content/docs/ja/{agents,integrations}.mdx`
- 修改：`../herdr/docs/next/website/src/content/docs/zh-cn/{agents,integrations}.mdx`
- 修改：`../herdr/src/agent_resume.rs`
- 修改：`../herdr/src/api/schema/integrations.rs`
- 修改：`../herdr/src/cli/integration.rs`
- 修改：`../herdr/src/integration/{actions,registry,targets}.rs`
- 修改：`../herdr/src/plugin_command.rs`

- [ ] **步骤 1：确认远端、工作区和合并输入**

运行：

```bash
git -C ../herdr status --short --branch
git -C ../herdr fetch --tags origin
git -C ../herdr rev-parse master origin/master 'v0.8.0^{}'
git -C ../herdr merge-base master 'v0.8.0^{}'
```

预期：工作区干净，`master` 与 `origin/master` 一致，`v0.8.0` 可解析。

- [ ] **步骤 2：创建普通 merge 并停在冲突状态**

运行：

```bash
git -C ../herdr merge --no-ff --no-commit 'v0.8.0^{}'
```

预期：Git 报告已知冲突；不执行 rebase、force push 或历史改写。

- [ ] **步骤 3：解决运行时代码冲突**

解决规则：

- `src/agent_resume.rs`、`src/integration/*.rs`、`src/cli/integration.rs` 和 `src/plugin_command.rs` 以官方 0.8.0 类型与接口为底稿，再补回 `IntegrationTarget::Csc`、CSC 检测与恢复路径。
- 保留 fork 的 `src/sidecar.rs`、`[[sidecar]]` 配置解析和 Herdr 生命周期守护；不得把 Pal 常驻逻辑移入 Herdr。
- 检查 `src/api/server/pane_graphics_stream.rs`：确保客户端注册完成后再发送图形流确认；若官方代码已等价实现，只保留一份逻辑。
- 所有 `match IntegrationTarget` 分支显式覆盖 `Csc`，不得使用会掩盖遗漏的兜底分支。

运行：

```bash
git -C ../herdr diff --name-only --diff-filter=U
rg -n 'Csc|csc|Sidecar|sidecar' ../herdr/src ../herdr/tests
```

预期：代码冲突全部清除，CSC 和 Sidecar 的实现、配置与测试仍可检索到。

- [ ] **步骤 4：解决 Schema 与文档冲突**

解决规则：

- Schema 以最终 Rust 类型重新生成为准，protocol 必须是 `19`，不得手工保留 protocol 17 Schema。
- changelog 和多语言网站文档保留官方 0.8.0 内容，再补回 CSC 与 Sidecar 的 fork 专属说明。
- 删除全部冲突标记，并检查没有意外删除 fork 专属文件。

运行：

```bash
rg -n '^(<<<<<<<|=======|>>>>>>>)' ../herdr
git -C ../herdr diff --name-only --diff-filter=U
```

预期：两条命令均无冲突输出。

- [ ] **步骤 5：执行 Herdr 完整验证**

运行：

```bash
(cd ../herdr && just check)
(cd ../herdr && ./unittest.sh)
rg -n '"protocol"[[:space:]]*:[[:space:]]*19' \
  ../herdr/docs/next/api/herdr-api.schema.json
```

预期：所有检查和 fork 定向测试通过，生成 Schema 为 protocol 19。

- [ ] **步骤 6：提交 Herdr merge**

运行：

```bash
git -C ../herdr add -A
git -C ../herdr commit -m 'merge: 合入 Herdr v0.8.0'
```

预期：生成双亲 merge commit，工作区恢复干净。

### 任务 3：修正 herdr-pal 的兼容性说明

**文件：**

- 修改：`README.md`
- 新建：`internal/buildscript/readme_test.go`
- 测试：`internal/herdr/client_test.go`
- 测试：`internal/integration/real_herdr_test.go`

- [ ] **步骤 1：增加 README 兼容性回归测试**

新建 `internal/buildscript/readme_test.go`：

```go
package buildscript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadmeDocumentsAuditedHerdrProtocols(t *testing.T) {
	root := repositoryRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("读取 README.md：%v", err)
	}
	readme := string(content)
	if !strings.Contains(readme, "已审计的 `17` 或 `19`") {
		t.Fatal("README.md 未同时说明已审计的 protocol 17 和 19")
	}
	if strings.Contains(readme, "protocol 必须为 `17`") {
		t.Fatal("README.md 仍把 protocol 17 描述为唯一兼容版本")
	}
}
```

运行：

```bash
go test ./internal/buildscript ./internal/herdr ./internal/integration
```

预期：新增断言先因 README 顶部旧描述失败。

- [ ] **步骤 2：修正 README**

将：

```markdown
- Herdr 公共 Socket API protocol 必须为 `17`。
```

替换为：

```markdown
- Herdr 公共 Socket API protocol 必须为已审计的 `17` 或 `19`。
```

并确认安装、排障章节与此描述一致。

- [ ] **步骤 3：验证定向测试**

运行：

```bash
go test ./internal/buildscript ./internal/herdr ./internal/integration
```

预期：协议 17、19 均通过，18 和未知更高协议仍被拒绝，README 回归断言通过。

### 任务 4：构建和验证四个平台 Herdr

**文件：**

- 生成：`../herdr/target/*/release/herdr`

- [ ] **步骤 1：安装缺失 macOS Rust 目标并确认 Docker**

运行：

```bash
rustup target add x86_64-apple-darwin
docker version
```

预期：macOS 两个 Rust 目标可用，Docker daemon 可用。

- [ ] **步骤 2：构建 macOS 两个架构**

运行：

```bash
cargo build --manifest-path ../herdr/Cargo.toml --release --target aarch64-apple-darwin
cargo build --manifest-path ../herdr/Cargo.toml --release --target x86_64-apple-darwin
```

预期：两个 `herdr` release 二进制生成成功。

- [ ] **步骤 3：在目标架构 Linux 容器中构建 musl 二进制**

先创建仅用于发布构建的临时输出目录：

```bash
mkdir -p dist/herdr-v0.8.0-linux-amd64 dist/herdr-v0.8.0-linux-arm64
```

分别使用 `linux/amd64` 和 `linux/arm64` 容器，按官方 Herdr release workflow 安装
Rust 1.96.1、Zig 0.15.2、CMake、Ninja 和 musl-tools。源码以只读方式挂载，容器通过
`git archive` 复制已提交的 `HEAD` 后构建：

```bash
docker run --rm --platform linux/amd64 \
  -v "$(cd ../herdr && pwd):/src:ro" \
  -v "$(pwd)/dist/herdr-v0.8.0-linux-amd64:/out" \
  rust:1.96.1-bookworm bash -lc '
    set -euo pipefail
    apt-get update
    apt-get install -y --no-install-recommends cmake ninja-build musl-tools curl xz-utils git ca-certificates
    curl -fsSL https://ziglang.org/download/0.15.2/zig-x86_64-linux-0.15.2.tar.xz -o /tmp/zig.tar.xz
    mkdir -p /opt/zig /work
    tar -xJf /tmp/zig.tar.xz -C /opt/zig --strip-components=1
    export PATH=/opt/zig:$PATH
    rustup target add x86_64-unknown-linux-musl
    git config --global --add safe.directory /src
    git -C /src archive HEAD | tar -x -C /work
    cd /work
    export LIBGHOSTTY_VT_OPTIMIZE=ReleaseFast LIBGHOSTTY_VT_SIMD=true
    cargo build --release --locked --target x86_64-unknown-linux-musl
    cp target/x86_64-unknown-linux-musl/release/herdr /out/herdr
  '

docker run --rm --platform linux/arm64 \
  -v "$(cd ../herdr && pwd):/src:ro" \
  -v "$(pwd)/dist/herdr-v0.8.0-linux-arm64:/out" \
  rust:1.96.1-bookworm bash -lc '
    set -euo pipefail
    apt-get update
    apt-get install -y --no-install-recommends cmake ninja-build musl-tools curl xz-utils git ca-certificates
    curl -fsSL https://ziglang.org/download/0.15.2/zig-aarch64-linux-0.15.2.tar.xz -o /tmp/zig.tar.xz
    mkdir -p /opt/zig /work
    tar -xJf /tmp/zig.tar.xz -C /opt/zig --strip-components=1
    export PATH=/opt/zig:$PATH
    rustup target add aarch64-unknown-linux-musl
    git config --global --add safe.directory /src
    git -C /src archive HEAD | tar -x -C /work
    cd /work
    export LIBGHOSTTY_VT_OPTIMIZE=ReleaseFast LIBGHOSTTY_VT_SIMD=true
    cargo build --release --locked --target aarch64-unknown-linux-musl
    cp target/aarch64-unknown-linux-musl/release/herdr /out/herdr
  '
```

将产物分别复制为：

```text
dist/herdr-v0.8.0-linux-amd64/herdr
dist/herdr-v0.8.0-linux-arm64/herdr
```

预期：容器架构与目标一致，生成 x86_64 与 aarch64 Linux musl release 二进制；容器不修改源码工作区。

- [ ] **步骤 4：验证架构、静态链接和本机 CLI**

运行：

```bash
file ../herdr/target/aarch64-apple-darwin/release/herdr
file ../herdr/target/x86_64-apple-darwin/release/herdr
file dist/herdr-v0.8.0-linux-amd64/herdr
file dist/herdr-v0.8.0-linux-arm64/herdr
../herdr/target/aarch64-apple-darwin/release/herdr --version
../herdr/target/aarch64-apple-darwin/release/herdr api schema --json
```

预期：架构与目标一致，Linux 为静态链接，本机 CLI 显示 0.8.0 系列版本且 Schema protocol 为 19。

### 任务 5：执行 herdr-pal 全量验证并提交

**文件：**

- 修改：`README.md`
- 修改：任务 3 新增的文档回归测试文件

- [ ] **步骤 1：运行完整单测和构建**

运行：

```bash
./unittest.sh
./build.sh
```

预期：Go 单测、race、vet 和全部跨平台 Pal/Server/CLI 构建通过。

- [ ] **步骤 2：使用合并后的 Herdr 执行只读联调**

使用临时配置和临时 Socket 启动本机 Herdr，不连接企业微信；依次验证 `ping`、`session.snapshot`、带 `pane_id` 的 `pane.agent_status_changed` 订阅，以及 `herdr-pal -i` 初始化。

预期：Pal 识别 protocol 19，快照和订阅确认可解析，退出后临时进程与文件全部清理。

- [ ] **步骤 3：提交 herdr-pal 改动**

运行：

```bash
git add README.md internal/buildscript/readme_test.go
git commit -m 'docs: 更新 Herdr 0.8.0 兼容说明'
```

预期：提交只包含协议说明与相应回归测试。

### 任务 6：构建并验证 v0.6.0 Bundle

**文件：**

- 生成：`dist/herdr-bundle-v0.6.0-{darwin-arm64,darwin-amd64,linux-amd64,linux-arm64}.tar.gz`
- 生成：上述四个压缩包对应的 `.sha256`

- [ ] **步骤 1：创建 annotated Tag 前确认提交和版本边界**

运行：

```bash
git status --short --branch
git log -3 --oneline
git tag -l v0.6.0
```

预期：工作区干净，`v0.6.0` 尚不存在。

- [ ] **步骤 2：创建本地 Tag**

运行：

```bash
git tag -a v0.6.0 -m 'v0.6.0'
```

预期：annotated Tag 指向已完整验证的 herdr-pal 提交。

- [ ] **步骤 3：为四个目标构建 Bundle**

依次运行：

```bash
./build.sh bundle --target darwin-arm64 --version v0.6.0 --herdr-source ../herdr
./build.sh bundle --target darwin-amd64 --version v0.6.0 --herdr-source ../herdr
./build.sh bundle --target linux-amd64 --version v0.6.0 \
  --herdr-binary dist/herdr-v0.8.0-linux-amd64/herdr \
  --herdr-commit "$(git -C ../herdr rev-parse --short HEAD)"
./build.sh bundle --target linux-arm64 --version v0.6.0 \
  --herdr-binary dist/herdr-v0.8.0-linux-arm64/herdr \
  --herdr-commit "$(git -C ../herdr rev-parse --short HEAD)"
```

预期：生成四个压缩包和四个校验文件，Bundle README 记录 Herdr merge commit。

- [ ] **步骤 4：验证全部 Bundle**

对四个平台逐一执行：

```bash
shasum -a 256 -c dist/herdr-bundle-v0.6.0-<目标>.tar.gz.sha256
tar -tzf dist/herdr-bundle-v0.6.0-<目标>.tar.gz
```

解包到 `mktemp -d` 创建的临时目录，使用 `file` 检查两个二进制架构；对 darwin-arm64 运行 `herdr --version`、`herdr api schema --json`、`herdr-pal --version`，并用临时 HOME 模拟 `install.sh`。

预期：所有校验和、清单、架构、版本和安装模拟通过，真实用户配置不被修改。

### 任务 7：推送并发布

**文件：** 无新增源码文件。

- [ ] **步骤 1：发布前最终验证**

运行：

```bash
(cd ../herdr && just check && ./unittest.sh)
./unittest.sh
./build.sh
git status --short --branch
git -C ../herdr status --short --branch
```

预期：全部验证通过，两个仓库均无未提交改动。

- [ ] **步骤 2：推送 Herdr fork**

运行：

```bash
git -C ../herdr push origin master
```

预期：远端 `master` 快进到本次 merge commit，不创建 Herdr Tag 或 Release。

- [ ] **步骤 3：推送 herdr-pal 和 Tag**

运行：

```bash
git push origin main
git push origin v0.6.0
```

预期：远端 `main` 和 `v0.6.0` 指向同一已验证提交。

- [ ] **步骤 4：创建 GitHub Release**

运行：

```bash
gh release create v0.6.0 \
  dist/herdr-bundle-v0.6.0-darwin-arm64.tar.gz \
  dist/herdr-bundle-v0.6.0-darwin-arm64.tar.gz.sha256 \
  dist/herdr-bundle-v0.6.0-darwin-amd64.tar.gz \
  dist/herdr-bundle-v0.6.0-darwin-amd64.tar.gz.sha256 \
  dist/herdr-bundle-v0.6.0-linux-amd64.tar.gz \
  dist/herdr-bundle-v0.6.0-linux-amd64.tar.gz.sha256 \
  dist/herdr-bundle-v0.6.0-linux-arm64.tar.gz \
  dist/herdr-bundle-v0.6.0-linux-arm64.tar.gz.sha256 \
  --title 'v0.6.0' \
  --generate-notes
```

预期：Release 不是 draft 或 prerelease，只包含约定的八个资产。

- [ ] **步骤 5：回读远端发布状态**

运行：

```bash
gh release view v0.6.0 --json tagName,isDraft,isPrerelease,url,assets
git ls-remote origin refs/heads/main refs/tags/v0.6.0
git -C ../herdr ls-remote origin refs/heads/master
```

预期：Tag、分支和 Release 一致，资产数量为八，文件名和大小均有效。
