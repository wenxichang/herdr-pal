# Herdr Pal 独立安装包 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. 本项目明确禁止使用 subagent-driven development。

**Goal:** 把 Release Bundle 攺为仅携带 Herdr Pal，并让安装器基于固定兼容名单发现、下载、校验和安装 Herdr。

**Architecture:** `packaging/build-bundle.sh` 负责把目标平台对应的 Herdr 兼容元数据写入安装器；`packaging/bundle/install.sh` 负责发现本机 Herdr、执行版本与 Schema 协议门禁，并在需要时下载固定官方二进制。已有外部安装只复用不覆盖，所有下载和替换在 Pal 配置变更前完成。

**Tech Stack:** POSIX shell、Go `testing`/`os/exec`、tar、curl、SHA-256、Herdr 公共 CLI。

---

### Task 1: 固定 Pal-only Bundle 结构

**Files:**
- Modify: `internal/buildscript/bundle_test.go`
- Modify: `packaging/build-bundle.sh`
- Modify: `packaging/bundle/README.md`
- Modify: `build.sh`

- [ ] **Step 1: 写入失败的 Bundle 结构测试**

把 `TestBuildBundlePackagesReleaseMatrix` 改为只准备 `dist/herdr-pal-<target>`，使用：

```go
command := exec.Command("/bin/sh", filepath.Join(root, "packaging", "build-bundle.sh"),
    "--target", target.name,
    "--version", "v0.5.0",
)
```

断言归档名为 `herdr-pal-bundle-v0.5.0-<target>.tar.gz`，包含 `herdr-pal`、`install.sh`、
`README.md`，且不包含 `/herdr`。同时断言旧参数 `--herdr-source`、`--herdr-binary`、
`--herdr-commit` 返回用法错误。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/buildscript -run 'TestBuildBundle|TestBuildScriptBundle' -count=1`

Expected: FAIL，旧脚本仍要求或打包 Herdr，并生成旧归档名。

- [ ] **Step 3: 实现最小 Pal-only 打包逻辑**

把构建脚本参数收敛为：

```sh
usage() {
    printf '%s\n' "用法: ./packaging/build-bundle.sh --target <linux-amd64|linux-arm64|darwin-amd64|darwin-arm64> --version <版本>" >&2
}
```

删除 Rust target、Herdr 源码构建和 Herdr 文件校验，只校验目标 Pal 二进制。Bundle 名称使用：

```sh
bundle_name=herdr-pal-bundle-$version-$target
```

README 模板改为说明安装包只携带 Pal，安装器会复用或下载兼容 Herdr。

- [ ] **Step 4: 运行 Bundle 测试确认通过**

Run: `go test ./internal/buildscript -run 'TestBuildBundle|TestBuildScriptBundle' -count=1`

Expected: PASS。

### Task 2: 实现 Herdr 发现和兼容门禁

**Files:**
- Modify: `internal/buildscript/install_test.go`
- Modify: `packaging/bundle/install.sh`
- Modify: `packaging/build-bundle.sh`

- [ ] **Step 1: 写入兼容版本复用测试**

测试创建一个 `PATH` 中的假 Herdr：

```sh
case "$*" in
  "--version") printf '%s\n' 'herdr 0.7.5' ;;
  "api schema --json") printf '%s\n' '{"protocol":17}' ;;
  "config check") exit 0 ;;
  "status server --json") printf '%s\n' '{"running":false}' ;;
  *) exit 1 ;;
esac
```

断言安装成功、没有调用下载器、`herdr-pal setup --herdr-bin` 指向该兼容绝对路径，并且安装
目录中没有新建 `herdr`。

- [ ] **Step 2: 运行兼容复用测试确认失败**

Run: `go test ./internal/buildscript -run TestBundleInstallScriptReusesCompatibleHerdr -count=1`

Expected: FAIL，旧安装器仍要求 Bundle 自带 Herdr。

- [ ] **Step 3: 实现候选发现和严格名单校验**

构建脚本为安装器替换以下常量：

```sh
HERDR_VERSION='0.7.5'
HERDR_PROTOCOL='17'
HERDR_DOWNLOAD_URL='@HERDR_DOWNLOAD_URL@'
HERDR_SHA256='@HERDR_SHA256@'
```

安装器新增 `inspect_herdr`，通过 `--version` 和 `api schema --json` 提取版本与协议；只接受
精确的 `0.7.5 + protocol 17`。候选顺序为 `<install_dir>/herdr`、`command -v herdr`，使用
物理绝对路径去重。

- [ ] **Step 4: 写入不兼容版本确认测试**

新增两组测试：输入 `n` 时断言安装器失败且没有 Pal/配置副作用；输入空行或 `y` 时进入下载
流程。诊断输出必须包含候选路径、版本和协议。

- [ ] **Step 5: 运行门禁测试确认通过**

Run: `go test ./internal/buildscript -run 'TestBundleInstallScript(Reuses|RejectsUnsupported|AcceptsUnsupported)' -count=1`

Expected: PASS。

### Task 3: 实现固定资产下载、验证和回滚

**Files:**
- Modify: `internal/buildscript/install_test.go`
- Modify: `internal/buildscript/bundle_test.go`
- Modify: `packaging/bundle/install.sh`
- Modify: `packaging/build-bundle.sh`

- [ ] **Step 1: 写入下载成功和失败测试**

在测试 `PATH` 中放置假 `curl`，按 `-o` 参数复制本地假 Herdr；安装模板注入测试 URL 和按
测试文件计算的 SHA-256。覆盖：Herdr 缺失自动下载、摘要错误、版本错误、协议错误和下载器
失败。

- [ ] **Step 2: 运行下载测试确认失败**

Run: `go test ./internal/buildscript -run 'TestBundleInstallScript(Downloads|RejectsDownload)' -count=1`

Expected: FAIL，下载函数尚不存在。

- [ ] **Step 3: 实现安全下载和验证**

新增 `download_herdr`：

```sh
curl -fL --retry 3 --connect-timeout 10 --max-time 120 \
    "$HERDR_DOWNLOAD_URL" -o "$download_path"
```

随后计算 SHA-256、调用 `file` 检查目标平台，赋予临时执行权限，再复用 `inspect_herdr` 验证
版本与协议。所有临时内容保存在 `mktemp -d` 中并由 trap 清理。

- [ ] **Step 4: 实现安装事务和回滚测试**

安装顺序为“准备并验证 Herdr → 安装 Herdr（仅需要时）→ 安装 Pal → setup”。Pal 安装或
setup 失败时恢复本次替换的 `<install_dir>/herdr` 和 `herdr-pal`；外部复用 Herdr 不参与
回滚。

- [ ] **Step 5: 运行全部安装脚本测试**

Run: `go test ./internal/buildscript -count=1`

Expected: PASS。

### Task 4: 更新用户文档和帮助文本

**Files:**
- Modify: `README.md`
- Modify: `help.md`
- Modify: `internal/server/default_help.md`
- Modify: `internal/server/help_test.go`
- Modify: `internal/server/router_test.go`
- Modify: `docs/HANDOFF_CONTEXT.md`
- Modify: `docs/BRIDGE_ARCHITECTURE.md`

- [ ] **Step 1: 先修改帮助测试为新文案**

断言帮助包含：

```text
herdr-pal-bundle-<版本>-<系统>-<架构>.tar.gz
安装包只携带 Herdr Pal
缺少 Herdr 时自动下载兼容版本
检测到不兼容 Herdr 时先询问
```

并把 `herdr-bundle-`、`Herdr 与 herdr-pal 会安装在同一目录` 加入禁止项。

- [ ] **Step 2: 运行帮助测试确认失败**

Run: `go test ./internal/server -run 'TestDefaultHelp|TestRouterAllowsHelp' -count=1`

Expected: FAIL，当前帮助仍描述旧 Bundle。

- [ ] **Step 3: 更新 README、help 和架构说明**

构建示例改为：

```sh
./build.sh bundle --target darwin-arm64 --version "$(git describe --tags --always)"
```

用户安装说明改用新归档名，并说明兼容版本复用、缺失自动下载、不兼容确认以及包管理器安装
不会被覆盖。维护文档删除“一体化包同时包含 Herdr”的表述。

- [ ] **Step 4: 运行文档相关测试**

Run: `go test ./internal/server ./internal/buildscript -count=1`

Expected: PASS。

### Task 5: 完整验证和提交

**Files:**
- Verify: all modified files

- [ ] **Step 1: 格式和差异检查**

Run: `gofmt -w internal/buildscript/bundle_test.go internal/buildscript/install_test.go internal/server/help_test.go internal/server/router_test.go`

Run: `git diff --check`

Expected: 无输出、退出码 0。

- [ ] **Step 2: 运行完整单元测试**

Run: `./unittest.sh`

Expected: 全部包 PASS。

- [ ] **Step 3: 运行完整构建**

Run: `./build.sh`

Expected: 退出码 0，并更新所有目标二进制和校验文件。

- [ ] **Step 4: 生成一个真实 Pal-only Bundle 做结构检查**

Run: `./build.sh bundle --target darwin-arm64 --version test-pal-only`

Expected: 生成 `dist/herdr-pal-bundle-test-pal-only-darwin-arm64.tar.gz`，归档中没有 Herdr
二进制，安装器中的目标元数据没有未替换占位符。

- [ ] **Step 5: 提交实现**

```sh
git add build.sh packaging internal README.md help.md docs
git commit -m "feat: 支持按兼容名单安装 Herdr"
```
