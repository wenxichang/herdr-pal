# Herdr Sidecar 一键安装包实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 构建包含 Herdr、Herdr Pal 和交互式 `install.sh` 的 Linux/macOS 一体化安装包，并让 Sidecar 部署无需手工配置 Socket。

**Architecture:** Herdr Pal 增加仅供安装器调用的 `setup` 子命令，负责结构化合并 JSON/TOML、备份、校验和原子写入；POSIX `install.sh` 只处理交互、二进制安装和 `live-handoff`。打包脚本按目标平台组合本地 Herdr 源码或预构建 Herdr 二进制与现有 Go 交叉编译产物，服务端默认帮助改为嵌入式 Markdown 并介绍一体化安装流程。

**Tech Stack:** Go 1.26、POSIX shell、Rust/Cargo、BurntSushi TOML、GitHub CLI、`tar`、SHA-256

---

项目规则禁止使用 subagent-driven 执行，因此本计划使用 `superpowers:executing-plans` 在当前会话逐项实施。

## 文件结构

- `internal/herdr/resolver.go`：增加 Sidecar 环境变量来源，保持 Socket 解析职责集中。
- `internal/installer/client_config.go`：合并并校验 Herdr Pal JSON 配置。
- `internal/installer/herdr_config.go`：保留原文地管理 Herdr Sidecar TOML 块。
- `internal/installer/files.go`：私有目录、备份、原子写入和失败恢复。
- `internal/installer/installer.go`：组合两类配置并调用 Herdr 公共配置校验命令。
- `cmd/herdr-pal/setup.go`：实现安装器专用 `setup` 子命令，Key 只从 stdin 读取。
- `packaging/bundle/install.sh`：平台包中的交互式安装器模板。
- `packaging/bundle/README.md`：平台包内的离线快速说明模板。
- `packaging/build-bundle.sh`：构建或接收 Herdr 二进制并组装单个平台包。
- `internal/buildscript/*_test.go`：覆盖安装脚本和打包脚本，不访问 IM 网络。
- `internal/server/default_help.md`：服务端首次创建运行时 `help.md` 的默认内容。
- `build.sh`：保留现有常规构建，增加 `bundle` 稳定入口。

### Task 1：让 Pal 优先使用 Sidecar 注入的 Socket

**Files:**
- Modify: `internal/herdr/resolver.go`
- Modify: `internal/herdr/resolver_test.go`
- Modify: `internal/app/relay.go`

- [ ] **Step 1：编写 Socket 来源优先级失败测试**

在 `internal/herdr/resolver_test.go` 增加：

```go
func TestResolveSocketWithEnvironmentUsesSidecarPathBeforeCLI(t *testing.T) {
	runner := &fakeCommandRunner{}

	path, err := ResolveSocketWithEnvironment(
		context.Background(), "", "/tmp/sidecar.sock", "named", runner,
	)

	if err != nil {
		t.Fatalf("ResolveSocketWithEnvironment() error = %v", err)
	}
	if path != "/tmp/sidecar.sock" {
		t.Fatalf("path = %q, want /tmp/sidecar.sock", path)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("CLI calls = %d, want 0", len(runner.calls))
	}
}

func TestResolveSocketWithEnvironmentKeepsExplicitPathFirst(t *testing.T) {
	path, err := ResolveSocketWithEnvironment(
		context.Background(), "/tmp/config.sock", "/tmp/sidecar.sock", "", &fakeCommandRunner{},
	)
	if err != nil || path != "/tmp/config.sock" {
		t.Fatalf("path = %q, error = %v", path, err)
	}
}
```

- [ ] **Step 2：运行测试并确认失败**

Run: `go test ./internal/herdr -run 'TestResolveSocketWithEnvironment'`

Expected: FAIL，提示 `ResolveSocketWithEnvironment` 未定义。

- [ ] **Step 3：实现新的解析入口**

在 `internal/herdr/resolver.go` 保留旧入口作为无环境值的包装，并增加：

```go
// ResolveSocketWithEnvironment 按显式配置、Sidecar 环境、公共 CLI 和默认路径解析 Socket。
func ResolveSocketWithEnvironment(ctx context.Context, explicitPath, environmentPath, sessionName string, runner CommandRunner) (string, error) {
	if explicitPath != "" {
		return explicitPath, nil
	}
	if environmentPath != "" {
		return environmentPath, nil
	}
	return resolveSocketWithoutHints(ctx, sessionName, runner)
}

// ResolveSocket 保持非 Sidecar 调用的原有行为。
func ResolveSocket(ctx context.Context, explicitPath, sessionName string, runner CommandRunner) (string, error) {
	return ResolveSocketWithEnvironment(ctx, explicitPath, "", sessionName, runner)
}
```

把原有 CLI/HOME 逻辑移入未导出的 `resolveSocketWithoutHints`，避免复制现有错误处理。

- [ ] **Step 4：在 Relay 启动层传入环境变量**

在 `internal/app/relay.go` 中使用可注入的 `options.Getenv`：

```go
getenv := options.Getenv
if getenv == nil {
	getenv = os.Getenv
}
socketPath, err := herdr.ResolveSocketWithEnvironment(
	ctx,
	loaded.Herdr.SocketPath,
	strings.TrimSpace(getenv("HERDR_SOCKET_PATH")),
	loaded.Herdr.Session,
	runner,
)
```

- [ ] **Step 5：运行 Herdr 与 App 测试**

Run: `go test ./internal/herdr ./internal/app`

Expected: PASS。

- [ ] **Step 6：提交 Socket 接入**

```bash
git add internal/herdr/resolver.go internal/herdr/resolver_test.go internal/app/relay.go
git commit -m "feat: 接入 Herdr Sidecar Socket 环境"
```

### Task 2：实现客户端配置合并和私有原子写入

**Files:**
- Create: `internal/installer/client_config.go`
- Create: `internal/installer/client_config_test.go`
- Create: `internal/installer/files.go`
- Create: `internal/installer/files_test.go`

- [ ] **Step 1：编写客户端配置合并失败测试**

覆盖新建配置和保留已有配置：

```go
func TestMergeClientConfigPreservesNonRelaySettings(t *testing.T) {
	existing := []byte(`{
  "relay":{"url":"wss://old.example/hprp","key":"hpk_1_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","skip_verify":false},
  "herdr":{"session":"work","socket_path":"/tmp/custom.sock"},
  "log":{"level":"debug"}
}`)

	merged, err := mergeClientConfig(existing, "wss://new.example/hprp", "hpk_2_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	var got config.ClientConfig
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatal(err)
	}
	if got.Relay.URL != "wss://new.example/hprp" || got.Relay.SkipVerify {
		t.Fatalf("relay = %+v", got.Relay)
	}
	if got.Herdr.Session != "work" || got.Herdr.SocketPath != "/tmp/custom.sock" || got.Log.Level != "debug" {
		t.Fatalf("non-relay settings changed: %+v", got)
	}
}
```

再覆盖空文件生成 `relay.skip_verify=true`、`herdr={}`、`log.level=info`，以及非法 `http://` URL、非法 Key 和多余 JSON 值。

- [ ] **Step 2：运行合并测试并确认失败**

Run: `go test ./internal/installer -run 'TestMergeClientConfig'`

Expected: FAIL，包或函数尚不存在。

- [ ] **Step 3：实现 JSON 合并**

`mergeClientConfig` 使用 `map[string]json.RawMessage` 保留已有字段，只替换 `relay.url` 和
`relay.key`；仅在字段缺失时补 `skip_verify=true`、空 `herdr` 和 `log.level=info`。输出使用
`json.MarshalIndent`，并通过临时文件调用 `config.LoadClient` 复核当前版本可启动。

核心签名：

```go
func mergeClientConfig(existing []byte, relayURL, relayKey string) ([]byte, error)
```

- [ ] **Step 4：编写原子写入和备份失败测试**

覆盖：创建目录权限不宽于 `0700`、文件权限 `0600`、已有文件产生时间戳备份、符号链接被
拒绝、写入失败后原文件保持不变。

```go
func TestWritePrivateFileBacksUpAndAtomicallyReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { t.Fatal(err) }
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil { t.Fatal(err) }

	backup, err := writePrivateFile(path, []byte("new"), time.Date(2026, 7, 31, 1, 2, 3, 0, time.UTC))
	if err != nil { t.Fatal(err) }
	if backup == "" { t.Fatal("backup path is empty") }
	assertFileContent(t, path, "new")
	assertFileContent(t, backup, "old")
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil { t.Fatal(err) }
	if string(data) != want { t.Fatalf("%s = %q, want %q", path, data, want) }
}
```

- [ ] **Step 5：实现私有原子文件操作**

在 `internal/installer/files.go` 实现：

```go
func writePrivateFile(path string, data []byte, now time.Time) (backupPath string, err error)
func restoreBackup(path, backupPath string) error
```

实现必须使用目标目录内的 `os.CreateTemp`、`Chmod(0600)`、`Sync`、`Close`、`Rename`；通过
`Lstat` 拒绝符号链接；备份名使用 UTC 时间和进程 ID，避免同秒覆盖。

- [ ] **Step 6：运行安装配置基础测试**

Run: `go test ./internal/installer`

Expected: PASS。

- [ ] **Step 7：提交客户端配置能力**

```bash
git add internal/installer/client_config.go internal/installer/client_config_test.go internal/installer/files.go internal/installer/files_test.go
git commit -m "feat: 添加安装配置原子写入"
```

### Task 3：实现 Herdr Sidecar TOML 合并和事务式配置

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Create: `internal/installer/herdr_config.go`
- Create: `internal/installer/herdr_config_test.go`
- Create: `internal/installer/installer.go`
- Create: `internal/installer/installer_test.go`

- [ ] **Step 1：添加 TOML 解析依赖**

Run: `go get github.com/BurntSushi/toml@v1.5.0`

Expected: `go.mod` 和 `go.sum` 增加直接依赖。

- [ ] **Step 2：编写 Sidecar TOML 合并失败测试**

测试常量和期望块：

```go
const expectedManagedBlock = `# BEGIN HERDR PAL MANAGED SIDECAR
[[sidecar]]
command = ["herdr-pal"]
# END HERDR PAL MANAGED SIDECAR
`

func TestMergeHerdrConfigAppendsManagedSidecarOnce(t *testing.T) {
	first, err := mergeHerdrConfig([]byte("[ui]\nmouse_capture = true\n"))
	if err != nil { t.Fatal(err) }
	second, err := mergeHerdrConfig(first)
	if err != nil { t.Fatal(err) }
	if string(first) != string(second) || strings.Count(string(second), "[[sidecar]]") != 1 {
		t.Fatalf("merge is not idempotent:\n%s", second)
	}
}
```

另外覆盖已有未标记 `command=["herdr-pal"]` 时不重复、其他 Sidecar 保留、管理标记块更新、
损坏 TOML、单边标记和重复标记拒绝。

- [ ] **Step 3：运行 TOML 测试并确认失败**

Run: `go test ./internal/installer -run 'TestMergeHerdrConfig'`

Expected: FAIL，`mergeHerdrConfig` 未定义。

- [ ] **Step 4：实现保留原文的 TOML 合并**

`internal/installer/herdr_config.go` 使用 BurntSushi TOML 解码现有配置并读取：

```go
type herdrDocument struct {
	Sidecars []struct {
		Command []string `toml:"command"`
	} `toml:"sidecar"`
}
```

无管理标记且已有精确 `[]string{"herdr-pal"}` 时返回原文；否则在尾部追加管理块。已有一对
管理标记时只替换标记之间内容；标记缺失、顺序错误或出现多次时返回错误。合并后再次 TOML
解码，不能重新编码整份用户配置。

- [ ] **Step 5：编写事务式 Apply 失败测试**

定义公开内部接口：

```go
type Request struct {
	ClientConfigPath string
	HerdrConfigPath  string
	HerdrBinaryPath  string
	RelayURL         string
	RelayKey         string
}

type Result struct {
	ClientBackupPath string
	HerdrBackupPath  string
}

type Options struct {
	Now              func() time.Time
	CheckHerdrConfig func(context.Context, string, string) error
}

func Apply(ctx context.Context, request Request, options Options) (Result, error)
```

测试要求：两个候选配置先全部解析和校验，再写入；Herdr 校验失败不修改任一文件；第二个写入
失败时恢复第一个文件；错误文本包含路径但不包含完整 Key。

- [ ] **Step 6：实现候选配置校验和事务写入**

默认 `CheckHerdrConfig` 创建权限 `0600` 的候选 TOML，并执行：

```go
command := exec.CommandContext(ctx, herdrBinaryPath, "config", "check")
command.Env = append(os.Environ(), "HERDR_CONFIG_PATH="+candidatePath)
```

输出最多保留 8 KiB，错误不得包含 Key。候选客户端配置通过 `config.LoadClient` 校验。写入顺序
为 Herdr TOML、Pal JSON；后一步失败时使用备份恢复前一步。

- [ ] **Step 7：运行 installer 全部测试**

Run: `go test ./internal/installer`

Expected: PASS。

- [ ] **Step 8：提交 Herdr 配置合并**

```bash
git add go.mod go.sum internal/installer
git commit -m "feat: 管理 Herdr Sidecar 安装配置"
```

### Task 4：增加 `herdr-pal setup` 安装辅助命令

**Files:**
- Create: `cmd/herdr-pal/setup.go`
- Create: `cmd/herdr-pal/setup_test.go`
- Modify: `cmd/herdr-pal/main.go`
- Modify: `cmd/herdr-pal/main_test.go`

- [ ] **Step 1：编写 setup CLI 失败测试**

测试以下调用：

```text
herdr-pal setup --url wss://relay.example/hprp \
  --config /tmp/config.json \
  --herdr-config /tmp/config.toml \
  --herdr-bin /tmp/herdr
```

stdin 只包含一行机器 Key。测试断言 Request 字段正确、成功输出只包含两个配置路径；缺少 URL、
空 Key、Key 后出现额外非空行、缺少 `--herdr-bin` 或未知参数返回退出码 `2`，stderr 不包含 Key。

- [ ] **Step 2：运行 CLI 测试并确认失败**

Run: `go test ./cmd/herdr-pal -run 'TestRunSetup'`

Expected: FAIL，`runSetup` 未定义或 `setup` 被当作多余位置参数。

- [ ] **Step 3：实现 setup 子命令分派**

在 `main.go` 的 `run` 开头增加：

```go
if len(args) > 0 && args[0] == "setup" {
	return runSetup(ctx, args[1:], stdin, stdout, stderr, installer.Apply)
}
```

`setup.go` 使用独立 `flag.FlagSet`，Key 通过 `bufio.Reader` 从 stdin 读取，不接受命令行 Key
参数。默认客户端路径使用 `config.DefaultClientPath()`；Herdr 配置默认遵循
`XDG_CONFIG_HOME/herdr/config.toml`，否则使用 `~/.config/herdr/config.toml`。

- [ ] **Step 4：运行 herdr-pal CLI 测试**

Run: `go test ./cmd/herdr-pal`

Expected: PASS。

- [ ] **Step 5：提交安装辅助命令**

```bash
git add cmd/herdr-pal/main.go cmd/herdr-pal/main_test.go cmd/herdr-pal/setup.go cmd/herdr-pal/setup_test.go
git commit -m "feat: 添加 Pal 安装配置命令"
```

### Task 5：实现交互式 `install.sh`

**Files:**
- Create: `packaging/bundle/install.sh`
- Create: `internal/buildscript/install_test.go`

- [ ] **Step 1：编写安装脚本端到端失败测试**

测试在临时 bundle 中放置记录调用的假 `herdr` 和 `herdr-pal`，替换模板中的平台变量后执行：

```go
const validTestKey = "hpk_7_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

command := exec.Command("/bin/sh", installScript)
command.Env = append(os.Environ(), "HOME="+home, "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
command.Stdin = strings.NewReader("\nwss://relay.example/hprp\n" + validTestKey + "\n\n")
output, err := command.CombinedOutput()
```

断言默认安装到 `~/.local/bin`，两个文件是 `0755` 普通文件且相邻；Key 只通过 setup stdin
传递且不出现在 output；已存在文件产生备份；运行中的 Herdr 默认执行带 `--import-exe` 的
`server live-handoff`。另测用户自定义目录、平台不匹配、setup 失败和拒绝 handoff。

- [ ] **Step 2：运行安装脚本测试并确认失败**

Run: `go test ./internal/buildscript -run 'TestBundleInstallScript'`

Expected: FAIL，模板不存在。

- [ ] **Step 3：实现 POSIX 安装脚本模板**

模板固定包含并由打包脚本替换：

```sh
BUNDLE_OS='@BUNDLE_OS@'
BUNDLE_ARCH='@BUNDLE_ARCH@'
BUNDLE_VERSION='@BUNDLE_VERSION@'
```

脚本必须：

- 将 `Darwin/Linux` 和 `x86_64/amd64/arm64/aarch64` 规范化后校验。
- 默认安装目录为 `$HOME/.local/bin`，支持输入 `~`、`~/path` 或绝对路径。
- TTY 下用 `stty -echo` 读取 Key，并用 trap 保证恢复；管道测试输入下直接 `read`。
- 使用目标目录临时文件、`chmod 0755` 和 `mv` 原子替换二进制，已有文件先备份。
- 调用已安装的 `herdr-pal setup --url ... --config ... --herdr-config ... --herdr-bin ...`，通过
  stdin 传 Key。
- 使用 `HERDR_CONFIG_PATH=... herdr config check` 做最终复核。
- 用 `herdr status server --json` 检测默认 Server；用户确认后执行
  `herdr server live-handoff --import-exe <安装目录>/herdr`。
- 安装目录不在 `PATH` 时打印当前 shell 可直接复制的 PATH 配置提示，但不自动修改 rc 文件。
- 不调用 `sudo`，不静默修改 shell rc 文件。

- [ ] **Step 4：运行 shell 语法与端到端测试**

Run: `/bin/sh -n packaging/bundle/install.sh && go test ./internal/buildscript -run 'TestBundleInstallScript'`

Expected: PASS。

- [ ] **Step 5：提交交互安装脚本**

```bash
git add packaging/bundle/install.sh internal/buildscript/install_test.go
git commit -m "feat: 添加 Sidecar 交互安装脚本"
```

### Task 6：实现四平台一体化打包

**Files:**
- Create: `packaging/bundle/README.md`
- Create: `packaging/build-bundle.sh`
- Create: `internal/buildscript/bundle_test.go`
- Modify: `internal/buildscript/build_test.go`
- Modify: `build.sh`

- [ ] **Step 1：编写打包矩阵失败测试**

表驱动测试四个目标映射：

```go
var bundleTargets = []struct {
	Name       string
	RustTarget string
	HerdrName  string
	PalName    string
}{
	{"linux-amd64", "x86_64-unknown-linux-musl", "herdr-linux-x86_64", "herdr-pal-linux-amd64"},
	{"linux-arm64", "aarch64-unknown-linux-musl", "herdr-linux-aarch64", "herdr-pal-linux-arm64"},
	{"darwin-amd64", "x86_64-apple-darwin", "herdr-macos-x86_64", "herdr-pal-darwin-amd64"},
	{"darwin-arm64", "aarch64-apple-darwin", "herdr-macos-aarch64", "herdr-pal-darwin-arm64"},
}
```

用假 `cargo`、`file` 和预构建 Herdr 文件运行脚本，断言包名、根目录、`herdr`、`herdr-pal`、
`install.sh`、`README.md`、文件权限和 `.sha256`。再测未知 target、目标不匹配、缺失二进制和
脏版本字符串拒绝。

- [ ] **Step 2：运行打包测试并确认失败**

Run: `go test ./internal/buildscript -run 'TestBuildBundle'`

Expected: FAIL，`packaging/build-bundle.sh` 不存在。

- [ ] **Step 3：实现单目标打包脚本**

稳定接口：

```text
./packaging/build-bundle.sh \
  --target darwin-arm64 \
  --version v0.5.0 \
  [--herdr-source /Users/wxc/Code/herdr | --herdr-binary /path/to/herdr]
```

未传 `--herdr-binary` 时，默认 Herdr 源码为 `${HERDR_SOURCE_DIR:-$HOME/Code/herdr}`，执行：

```sh
(cd "$herdr_source" && cargo build --release --locked --target "$rust_target")
```

Pal 使用 `dist/herdr-pal-<os>-<arch>`。脚本使用 `file` 校验 Mach-O/ELF 和架构，Linux 包额外
拒绝动态链接或未解析 C++ runtime 符号；临时目录使用 `mktemp -d` 并由 trap 清理。渲染安装
脚本和 README 后创建 `dist/herdr-bundle-<version>-<target>.tar.gz` 与同名 `.sha256`。

- [ ] **Step 4：给根构建脚本增加稳定入口**

在 `build.sh` 最前面处理：

```sh
if [ "${1-}" = "bundle" ]; then
	shift
	"$0"
	exec ./packaging/build-bundle.sh "$@"
fi
if [ "$#" -ne 0 ]; then
	printf '%s\n' "用法: ./build.sh [bundle <打包参数>]" >&2
	exit 2
fi
```

普通 `./build.sh` 行为保持不变；`./build.sh bundle ...` 先生成 Pal 发布矩阵，再组装一个目标包。

- [ ] **Step 5：运行构建脚本测试**

Run: `go test ./internal/buildscript`

Expected: PASS。

- [ ] **Step 6：提交打包能力**

```bash
git add build.sh packaging internal/buildscript/build_test.go internal/buildscript/bundle_test.go
git commit -m "feat: 构建 Herdr 一体化安装包"
```

### Task 7：替换 `/help` 安装说明并更新项目文档

**Files:**
- Create: `internal/server/default_help.md`
- Modify: `internal/server/router.go`
- Modify: `internal/server/help_test.go`
- Modify: `README.md`

- [ ] **Step 1：编写默认帮助内容失败测试**

在 `internal/server/help_test.go` 增加：

```go
func TestDefaultHelpUsesSidecarBundleInstallation(t *testing.T) {
	help := DefaultHelpText()
	for _, want := range []string{
		"herdr-bundle-<版本>-<系统>-<架构>.tar.gz",
		"./install.sh",
		"/userid",
		"/ls",
		"Sidecar",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("default help missing %q", want)
		}
	}
	for _, old := range []string{"curl -fsSL https://herdr.dev/install.sh", "创建 config.json", "herdr-pal-windows-amd64.exe"} {
		if strings.Contains(help, old) {
			t.Errorf("default help still contains old installation text %q", old)
		}
	}
}
```

- [ ] **Step 2：运行帮助测试并确认失败**

Run: `go test ./internal/server -run 'TestDefaultHelpUsesSidecarBundleInstallation'`

Expected: FAIL，仍包含旧的分别安装说明。

- [ ] **Step 3：把默认帮助迁移为嵌入式 Markdown**

创建 `internal/server/default_help.md`，保留基本控制、按键和模式帮助，将安装部分替换为：下载
匹配的 bundle、校验 SHA-256、执行 `./install.sh`、通过 `/userid` 获取签发信息、输入 URL/Key、
启动或 handoff Herdr、用 `/ls` 验证。删除 Windows 和手工 JSON/TOML 配置步骤。

`internal/server/router.go` 使用：

```go
import _ "embed"

//go:embed default_help.md
var serverHelpTextTemplate string
```

`DefaultHelpText` 返回嵌入内容，运行时 `help.md` 仍保持每次读取、不缓存。

- [ ] **Step 4：更新 README**

README 客户端部署入口改为 bundle 优先，列出四个文件名、`./install.sh`、默认安装目录、配置
备份、Sidecar 生命周期和高级手工配置入口。明确 Server 已有的
`~/.config/herdr-pal-server/help.md` 不会被升级覆盖，管理员需按新默认内容同步替换安装章节。

- [ ] **Step 5：运行帮助和文档测试**

Run: `go test ./internal/server ./internal/serverapp`

Expected: PASS。

- [ ] **Step 6：提交帮助与文档**

```bash
git add internal/server/default_help.md internal/server/router.go internal/server/help_test.go README.md
git commit -m "docs: 更新 Sidecar 一键安装指引"
```

### Task 8：完整验证并生成四个平台包

**Files:**
- Modify if required by verification only: files changed in Tasks 1-7
- Output: `dist/herdr-bundle-*.tar.gz`
- Output: `dist/herdr-bundle-*.tar.gz.sha256`

- [ ] **Step 1：执行格式、单测和常规构建**

Run:

```bash
./unittest.sh
./build.sh
```

Expected: 两个命令均退出 `0`；`gofmt`、`go vet`、普通测试、race 测试及发布矩阵全部通过。

- [ ] **Step 2：从 Herdr 当前 master 构建四个平台二进制**

确认 `/Users/wxc/Code/herdr` 的 HEAD 与 `origin/master` 一致，然后触发现有手工构建工作流：

```bash
gh workflow run build-artifacts-manual.yml \
  --repo wenxichang/herdr \
  -f build_group=all \
  -f libghostty_optimize=ReleaseFast \
  -f libghostty_simd=false
```

使用 `gh run watch` 等待成功，下载本次 run 的四个 Linux/macOS artifact；检查每份
`BUILD_INFO.txt` 的 commit 等于本地 Herdr HEAD。Windows artifact 不参与本次 bundle。

- [ ] **Step 3：组装四个平台包**

对四个下载二进制分别执行：

```bash
BUNDLE_VERSION="$(git describe --tags --always)"
case "$BUNDLE_VERSION" in *-dirty) echo "工作区不是可打包状态" >&2; exit 1;; esac
./build.sh bundle --target linux-amd64  --version "$BUNDLE_VERSION" --herdr-binary "$LINUX_AMD64_HERDR"
./build.sh bundle --target linux-arm64  --version "$BUNDLE_VERSION" --herdr-binary "$LINUX_ARM64_HERDR"
./build.sh bundle --target darwin-amd64 --version "$BUNDLE_VERSION" --herdr-binary "$DARWIN_AMD64_HERDR"
./build.sh bundle --target darwin-arm64 --version "$BUNDLE_VERSION" --herdr-binary "$DARWIN_ARM64_HERDR"
```

`BUNDLE_VERSION` 使用当前 Herdr Pal tag；没有精确 tag 时显式使用 `git describe --tags --always`
的非脏值并记录在交付说明中。

- [ ] **Step 4：检查包内容和校验和**

Run:

```bash
for archive in dist/herdr-bundle-*.tar.gz; do
  tar -tzf "$archive"
done
```

Expected: 每个包只有一个根目录，包含 `herdr`、`herdr-pal`、`install.sh`、`README.md`；四份
`.sha256` 均可在当前系统校验通过。

- [ ] **Step 5：执行 macOS 真实安装冒烟测试**

在临时 HOME 解压当前 macOS 架构包，通过 stdin 输入默认安装目录、测试 `wss://` URL、格式
有效的测试 Key，并选择不 handoff。验证：

- 两个真实二进制相邻且可执行。
- `herdr-pal setup` 生成权限 `0600` 的 JSON。
- Herdr TOML 只包含一个受管 Sidecar。
- 再执行一次安装后配置不重复、旧二进制和旧配置有备份。
- `HERDR_SOCKET_PATH=/tmp/test.sock <安装目录>/herdr-pal` 的启动日志表明采用环境 Socket；连接
  失败后停止测试进程，不能连接真实 Relay。

- [ ] **Step 6：最终差异和安全检查**

Run:

```bash
git diff --check
git status --short
rg -n 'hpk_[0-9]+_|wss://[^ ]+:[^ ]+@' --glob '!docs/**' --glob '!**/*_test.go' .
```

Expected: 无空白错误、无意外文件、无真实 Key 或 URL 凭据；只保留预期源码、文档和 dist
产物。

- [ ] **Step 7：如验证产生修正则提交**

仅在验证发现并修正问题时执行：

```bash
git add build.sh README.md cmd internal packaging go.mod go.sum
git commit -m "fix: 修正 Sidecar 安装包验证问题"
```

提交前重新运行 `./unittest.sh` 和 `./build.sh`。
