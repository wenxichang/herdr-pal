package dsh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testDarwinWebPID       = uint32(700)
	testDarwinDesktopPID   = uint32(800)
	testWebPort            = uint16(3080)
	testDesktopPort        = uint16(3090)
	testWebInstallDir      = "/Users/alice/.dsh-install"
	testDesktopInstallDir  = "/Applications/DeepSeek Harness.app"
	testWebPackageJSONPath = testWebInstallDir + "/package.json"
	testDesktopPlistPath   = testDesktopInstallDir + "/Contents/Info.plist"
)

// testFileInfo 是 fake Prober 使用的最小 os.FileInfo 实现。
type testFileInfo struct {
	mode os.FileMode
	mod  time.Time
}

func (info testFileInfo) Name() string       { return "test" }
func (info testFileInfo) Size() int64        { return 1 }
func (info testFileInfo) Mode() os.FileMode  { return info.mode }
func (info testFileInfo) ModTime() time.Time { return info.mod }
func (info testFileInfo) IsDir() bool        { return info.mode.IsDir() }
func (info testFileInfo) Sys() any           { return nil }

var testRegularFile = testFileInfo{mode: 0o644}

// fakeProber 按固定夹具响应发现流程的所有探测，避免测试读取真实进程。
type fakeProber struct {
	commands    map[string][]byte
	commandErrs map[string]error
	files       map[string][]byte
	fileErrs    map[string]error
	infos       map[string]os.FileInfo
	statErrs    map[string]error
	listeners   map[uint32][]uint16
	listenerErr map[uint32]error
	fences      map[uint16]bool
	calls       []string
}

func proberRunKey(name string, args ...string) string {
	return strings.Join(append([]string{name}, args...), "\x00")
}

func (fake *fakeProber) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := proberRunKey(name, args...)
	fake.calls = append(fake.calls, "run:"+key)
	if err := fake.commandErrs[key]; err != nil {
		return nil, err
	}
	if out, ok := fake.commands[key]; ok {
		return out, nil
	}
	return nil, fmt.Errorf("fake prober 未配置命令: %q", key)
}

func (fake *fakeProber) ReadFile(path string) ([]byte, error) {
	fake.calls = append(fake.calls, "read:"+path)
	if err := fake.fileErrs[path]; err != nil {
		return nil, err
	}
	if data, ok := fake.files[path]; ok {
		return data, nil
	}
	return nil, fmt.Errorf("fake prober 未配置文件: %q", path)
}

func (fake *fakeProber) Stat(path string) (os.FileInfo, error) {
	fake.calls = append(fake.calls, "stat:"+path)
	if err := fake.statErrs[path]; err != nil {
		return nil, err
	}
	if info, ok := fake.infos[path]; ok {
		return info, nil
	}
	return nil, fmt.Errorf("fake prober 未配置文件状态: %q", path)
}

func (fake *fakeProber) LocalListeners(pid uint32) ([]uint16, error) {
	fake.calls = append(fake.calls, "listeners:"+strconv.FormatUint(uint64(pid), 10))
	if err := fake.listenerErr[pid]; err != nil {
		return nil, err
	}
	if ports, ok := fake.listeners[pid]; ok {
		return ports, nil
	}
	return nil, fmt.Errorf("fake prober 未配置监听: %d", pid)
}

func (fake *fakeProber) Fence(port uint16) bool {
	fake.calls = append(fake.calls, "fence:"+strconv.Itoa(int(port)))
	return fake.fences[port]
}

// newDarwinFixture 构造一个同时运行 dsh web 与 Desktop 的 macOS 夹具。
func newDarwinFixture(t *testing.T) *fakeProber {
	t.Helper()
	uid := os.Getuid()
	processes := fmt.Sprintf(`  700   %d /usr/local/bin/node
  800   %d %s/Contents/MacOS/DeepSeek Harness
  900   %d /usr/bin/vim
`, uid, uid, testDesktopInstallDir, uid)
	webArgs := "node " + testWebInstallDir + "/lib/bin.js web --port 3080"
	desktopArgs := testDesktopInstallDir + "/Contents/MacOS/DeepSeek Harness" +
		" --user-data-dir=/Users/alice/Library/Application Support/DeepSeek Harness" +
		" " + testDesktopInstallDir + "/Contents/Resources/app.asar/node_modules/dsh/desktop-host/index.js" +
		" --profile profiles/desktop"
	return &fakeProber{
		commands: map[string][]byte{
			proberRunKey("ps", "-U", strconv.Itoa(uid), "-o", "pid=,uid=,comm="):                          []byte(processes),
			proberRunKey("ps", "-ww", "-p", "700", "-o", "args="):                                         []byte(webArgs),
			proberRunKey("ps", "-ww", "-p", "800", "-o", "args="):                                         []byte(desktopArgs),
			proberRunKey("ps", "-p", "700", "-o", "uid=,lstart="):                                         []byte(fmt.Sprintf("%d Mon Nov 10 16:00:00 2025", uid)),
			proberRunKey("ps", "-p", "800", "-o", "uid=,lstart="):                                         []byte(fmt.Sprintf("%d Mon Nov 10 15:00:00 2025", uid)),
			proberRunKey("plutil", "-extract", "CFBundleShortVersionString", "raw", testDesktopPlistPath): []byte("0.2.2-rc.1\n"),
		},
		files: map[string][]byte{
			testWebPackageJSONPath: []byte(`{"name":"dsh","version":"0.2.1-rc.5"}`),
		},
		infos: map[string]os.FileInfo{
			testWebPackageJSONPath: testRegularFile,
			testDesktopPlistPath:   testRegularFile,
		},
		listeners: map[uint32][]uint16{
			testDarwinWebPID:     {testWebPort},
			testDarwinDesktopPID: {testDesktopPort},
		},
		fences: map[uint16]bool{
			testWebPort:     true,
			testDesktopPort: true,
		},
	}
}

// stubProcessEnvironment 替换进程环境读取，使测试不依赖真实进程或身份。
func stubProcessEnvironment(t *testing.T, homes map[uint32]string) {
	t.Helper()
	previous := readProcessEnvironment
	readProcessEnvironment = func(pid uint32) []string {
		home, ok := homes[pid]
		if !ok {
			return nil
		}
		return []string{"PATH=/usr/bin", "DSH_HOME=" + home}
	}
	t.Cleanup(func() { readProcessEnvironment = previous })
}

func requireDarwinDiscovery(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("darwin 发现实现在非 darwin 构建中不可用")
	}
}

func testHostGeneration(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])[:24]
}

func TestDiscoverRecognizesWebAndDesktop(t *testing.T) {
	requireDarwinDiscovery(t)
	fake := newDarwinFixture(t)
	stubProcessEnvironment(t, map[uint32]string{
		testDarwinWebPID:     "/Users/alice/.dsh-shared",
		testDarwinDesktopPID: "/Users/alice/.dsh-shared",
	})

	hosts, err := discoverHosts(context.Background(), DiscoveryConfig{}, fake, "darwin")
	if err != nil {
		t.Fatalf("discoverHosts() error = %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("发现宿主数 = %d, want 2: %+v", len(hosts), hosts)
	}

	byVariant := make(map[string]Host, len(hosts))
	for _, host := range hosts {
		if err := host.Validate(); err != nil {
			t.Fatalf("发现宿主未通过校验: %+v, %v", host, err)
		}
		if len(host.Generation) != 24 {
			t.Fatalf("Generation 长度 = %d, want 24: %q", len(host.Generation), host.Generation)
		}
		byVariant[host.Variant] = host
	}

	web, ok := byVariant[variantWeb]
	if !ok {
		t.Fatal("缺少 web 宿主")
	}
	if web.PID != testDarwinWebPID || web.Port != testWebPort || web.Version != "0.2.1-rc.5" ||
		web.InstallDir != testWebInstallDir || web.Home != "/Users/alice/.dsh-shared" ||
		web.Executable != "/usr/local/bin/node" {
		t.Fatalf("web 宿主字段错误: %+v", web)
	}

	desktop, ok := byVariant[variantDesktop]
	if !ok {
		t.Fatal("缺少 desktop 宿主")
	}
	if desktop.PID != testDarwinDesktopPID || desktop.Port != testDesktopPort ||
		desktop.Version != "0.2.2-rc.1" || desktop.InstallDir != testDesktopInstallDir ||
		desktop.Home != "/Users/alice/.dsh-shared" {
		t.Fatalf("desktop 宿主字段错误: %+v", desktop)
	}

	if hosts[0].Reference() >= hosts[1].Reference() {
		t.Fatalf("宿主未按 Reference 排序: %q >= %q", hosts[0].Reference(), hosts[1].Reference())
	}
}

func TestDiscoverRejectsNonDSHListener(t *testing.T) {
	requireDarwinDiscovery(t)
	uid := os.Getuid()
	processes := fmt.Sprintf(`  700   %d /usr/local/bin/node
  800   %d %s/Contents/MacOS/DeepSeek Harness
  900   %d /usr/local/bin/node
  901   %d /usr/local/bin/node
  902   %d /usr/local/bin/node
`, uid, uid, testDesktopInstallDir, uid, uid, uid)
	fake := &fakeProber{
		commands: map[string][]byte{
			proberRunKey("ps", "-U", strconv.Itoa(uid), "-o", "pid=,uid=,comm="): []byte(processes),
			// 普通 node 进程。
			proberRunKey("ps", "-ww", "-p", "700", "-o", "args="): []byte("node /srv/not-dsh/server.js"),
			// 看起来像 Desktop，但缺少 desktop-host 入口。
			proberRunKey("ps", "-ww", "-p", "800", "-o", "args="): []byte(testDesktopInstallDir + "/Contents/MacOS/DeepSeek Harness --profile profiles/desktop"),
			// dsh web，但 401 fence 缺失。
			proberRunKey("ps", "-ww", "-p", "900", "-o", "args="): []byte("node /opt/old/lib/bin.js web"),
			proberRunKey("ps", "-p", "900", "-o", "uid=,lstart="): []byte(fmt.Sprintf("%d Mon Nov 10 16:00:00 2025", uid)),
			// dsh web，版本未知，应被版本门禁过滤。
			proberRunKey("ps", "-ww", "-p", "901", "-o", "args="): []byte("node /opt/new/lib/bin.js web"),
			proberRunKey("ps", "-p", "901", "-o", "uid=,lstart="): []byte(fmt.Sprintf("%d Mon Nov 10 16:00:00 2025", uid)),
			// 自动发现不接受显式配置专用的内部版本值。
			proberRunKey("ps", "-ww", "-p", "902", "-o", "args="): []byte("node /opt/explicit/lib/bin.js web"),
			proberRunKey("ps", "-p", "902", "-o", "uid=,lstart="): []byte(fmt.Sprintf("%d Mon Nov 10 16:00:00 2025", uid)),
		},
		files: map[string][]byte{
			"/opt/old/package.json":      []byte(`{"version":"0.2.1-rc.5"}`),
			"/opt/new/package.json":      []byte(`{"version":"0.2.3-rc.1"}`),
			"/opt/explicit/package.json": []byte(`{"version":"explicit"}`),
		},
		infos: map[string]os.FileInfo{
			"/opt/old/package.json":      testRegularFile,
			"/opt/new/package.json":      testRegularFile,
			"/opt/explicit/package.json": testRegularFile,
		},
		listeners: map[uint32][]uint16{
			900: {testWebPort},
			901: {testWebPort + 1},
			902: {testWebPort + 2},
		},
		fences: map[uint16]bool{
			testWebPort:     false,
			testWebPort + 1: true,
			testWebPort + 2: true,
		},
	}
	stubProcessEnvironment(t, nil)

	hosts, err := discoverHosts(context.Background(), DiscoveryConfig{}, fake, "darwin")
	if err != nil {
		t.Fatalf("discoverHosts() error = %v", err)
	}
	if len(hosts) != 0 {
		t.Fatalf("非 dsh 监听、fence 缺失和未知版本都应被过滤，got %+v", hosts)
	}
}

func TestDiscoverDedupesHostsAndAppliesVariantFilter(t *testing.T) {
	requireDarwinDiscovery(t)
	tests := []struct {
		name     string
		variant  string
		want     int
		variants []string
	}{
		{name: "全部宿主", variant: "", want: 2, variants: []string{variantWeb, variantDesktop}},
		{name: "auto 不过滤", variant: "auto", want: 2, variants: []string{variantWeb, variantDesktop}},
		{name: "只保留 web", variant: variantWeb, want: 1, variants: []string{variantWeb}},
		{name: "只保留 desktop", variant: variantDesktop, want: 1, variants: []string{variantDesktop}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newDarwinFixture(t)
			// 同一进程的重复监听必须只产生一个宿主。
			fake.listeners[testDarwinWebPID] = []uint16{testWebPort, testWebPort}
			stubProcessEnvironment(t, nil)

			hosts, err := discoverHosts(context.Background(), DiscoveryConfig{Variant: test.variant}, fake, "darwin")
			if err != nil {
				t.Fatalf("discoverHosts() error = %v", err)
			}
			if len(hosts) != test.want {
				t.Fatalf("宿主数 = %d, want %d: %+v", len(hosts), test.want, hosts)
			}
			seen := make(map[string]struct{}, len(hosts))
			gotVariants := make(map[string]bool, len(hosts))
			for _, host := range hosts {
				if _, duplicate := seen[host.Reference()]; duplicate {
					t.Fatalf("重复宿主引用: %q", host.Reference())
				}
				seen[host.Reference()] = struct{}{}
				gotVariants[host.Variant] = true
			}
			for _, variant := range test.variants {
				if !gotVariants[variant] {
					t.Fatalf("缺少变体 %q: %+v", variant, hosts)
				}
			}
		})
	}

	if _, err := discoverHosts(context.Background(), DiscoveryConfig{Variant: "cli"}, &fakeProber{}, "darwin"); err == nil {
		t.Fatal("未知变体应返回错误")
	}
}

func TestDiscoverHomeOverride(t *testing.T) {
	requireDarwinDiscovery(t)
	fake := newDarwinFixture(t)
	stubProcessEnvironment(t, map[uint32]string{
		testDarwinWebPID:     "/Users/alice/.dsh-env",
		testDarwinDesktopPID: "/Users/alice/.dsh-env",
	})

	hosts, err := discoverHosts(context.Background(), DiscoveryConfig{Home: "/srv/shared-dsh"}, fake, "darwin")
	if err != nil {
		t.Fatalf("discoverHosts() error = %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("宿主数 = %d, want 2", len(hosts))
	}
	for _, host := range hosts {
		if host.Home != "/srv/shared-dsh" {
			t.Fatalf("cfg.Home 未覆盖发现值: %+v", host)
		}
	}
}

func TestDiscoverDesktopVersionFallsBackToBundleVersion(t *testing.T) {
	requireDarwinDiscovery(t)
	fake := newDarwinFixture(t)
	// electron-builder 可能把不完整的版本放入 ShortVersionString，
	// 完整预发布版本位于 CFBundleVersion。
	fake.commands[proberRunKey("plutil", "-extract", "CFBundleShortVersionString", "raw", testDesktopPlistPath)] = []byte("0.2.2\n")
	fake.commands[proberRunKey("plutil", "-extract", "CFBundleVersion", "raw", testDesktopPlistPath)] = []byte("0.2.2-rc.1\n")
	stubProcessEnvironment(t, nil)

	hosts, err := discoverHosts(context.Background(), DiscoveryConfig{Variant: variantDesktop}, fake, "darwin")
	if err != nil {
		t.Fatalf("discoverHosts() error = %v", err)
	}
	if len(hosts) != 1 || hosts[0].Version != "0.2.2-rc.1" {
		t.Fatalf("Desktop 版本回退失败: %+v", hosts)
	}
}

func TestDiscoverExplicitURLSkipsProcessScan(t *testing.T) {
	explicitURL := "http://127.0.0.1:4010"
	explicitHome := "/Users/alice/custom-dsh"
	fake := &fakeProber{}

	hosts, err := discoverHosts(context.Background(), DiscoveryConfig{
		URL:     explicitURL,
		Home:    explicitHome,
		Variant: variantDesktop,
	}, fake, "darwin")
	if err != nil {
		t.Fatalf("discoverHosts() error = %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("显式配置不应扫描进程或探测端口: %v", fake.calls)
	}
	if len(hosts) != 1 {
		t.Fatalf("显式配置宿主数 = %d, want 1", len(hosts))
	}
	host := hosts[0]
	if host.PID != 0 || host.Port != 4010 || host.Version != versionExplicit ||
		host.Variant != variantDesktop || host.Home != explicitHome {
		t.Fatalf("显式宿主字段错误: %+v", host)
	}
	if want := testHostGeneration("explicit", explicitURL, explicitHome); host.Generation != want {
		t.Fatalf("Generation = %q, want %q", host.Generation, want)
	}
	if err := host.Validate(); err != nil {
		t.Fatalf("显式宿主未通过校验: %v", err)
	}

	t.Run("空变体默认 web", func(t *testing.T) {
		hosts, err := discoverHosts(context.Background(), DiscoveryConfig{URL: explicitURL, Home: explicitHome}, fake, "darwin")
		if err != nil {
			t.Fatalf("discoverHosts() error = %v", err)
		}
		if hosts[0].Variant != variantWeb {
			t.Fatalf("Variant = %q, want web", hosts[0].Variant)
		}
		if err := hosts[0].Validate(); err != nil {
			t.Fatalf("显式宿主未通过校验: %v", err)
		}
	})

	t.Run("auto 视为未设置", func(t *testing.T) {
		hosts, err := discoverHosts(context.Background(), DiscoveryConfig{URL: explicitURL, Home: explicitHome, Variant: "auto"}, fake, "darwin")
		if err != nil {
			t.Fatalf("discoverHosts() error = %v", err)
		}
		if hosts[0].Variant != variantWeb {
			t.Fatalf("Variant = %q, want web", hosts[0].Variant)
		}
	})

	t.Run("空 home 默认 ~/.dsh", func(t *testing.T) {
		userHome, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("UserHomeDir() error = %v", err)
		}
		hosts, err := discoverHosts(context.Background(), DiscoveryConfig{URL: explicitURL}, fake, "darwin")
		if err != nil {
			t.Fatalf("discoverHosts() error = %v", err)
		}
		wantHome := filepath.Join(userHome, ".dsh")
		if hosts[0].Home != wantHome {
			t.Fatalf("Home = %q, want %q", hosts[0].Home, wantHome)
		}
		if want := testHostGeneration("explicit", explicitURL, wantHome); hosts[0].Generation != want {
			t.Fatalf("Generation = %q, want %q", hosts[0].Generation, want)
		}
	})
}

func TestDiscoverRejectsInvalidExplicitConfig(t *testing.T) {
	fake := &fakeProber{}
	for _, cfg := range []DiscoveryConfig{
		{URL: "http://localhost:3080"},
		{URL: "https://127.0.0.1:3080"},
		{URL: "http://127.0.0.1"},
		{URL: "http://127.0.0.1:0"},
		{URL: "http://127.0.0.1:65536"},
		{URL: "http://127.0.0.1:3080/api"},
		{URL: "http://user@127.0.0.1:3080"},
		{URL: "http://127.0.0.1:3080?x=1"},
		{URL: "http://127.0.0.1:3080", Home: "relative-home"},
	} {
		if _, err := discoverHosts(context.Background(), cfg, fake, "darwin"); err == nil {
			t.Errorf("显式配置 %+v 应被拒绝", cfg)
		}
	}
	if len(fake.calls) != 0 {
		t.Fatalf("非法显式配置不应探测系统: %v", fake.calls)
	}
}

func TestDiscoverUnsupportedPlatformNeedsExplicitConfig(t *testing.T) {
	_, err := discoverHosts(context.Background(), DiscoveryConfig{}, &fakeProber{}, "windows")
	want := "当前平台需要显式配置 dsh.url 与 dsh.home"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestDiscoverLinuxCmdline(t *testing.T) {
	ssOutput := `LISTEN 0 4096 127.0.0.1:3080 0.0.0.0:* users:(("node",pid=700,fd=22))
LISTEN 0 4096 0.0.0.0:3081 0.0.0.0:* users:(("node",pid=701,fd=22))
LISTEN 0 4096 [::1]:3082 [::]:* users:(("node",pid=702,fd=22))
LISTEN 0 4096 127.0.0.1:3090 0.0.0.0:* users:(("node",pid=800,fd=23))
`
	// /proc/<pid>/stat 中 starttime 为第 22 个字段（右括号之后第 19 个）。
	statFixture := "700 (node) S 1 700 700 0 -1 4194560 100 0 0 0 5 10 0 0 20 0 8 0 987654 0 0\n"
	fake := &fakeProber{
		commands: map[string][]byte{
			proberRunKey("ss", "-H", "-ltnp"): []byte(ssOutput),
		},
		files: map[string][]byte{
			"/proc/700/cmdline":     []byte("node\x00/opt/dsh/lib/bin.js\x00web\x00"),
			"/proc/700/environ":     []byte("PATH=/usr/bin\x00DSH_HOME=/home/alice/.dsh\x00"),
			"/proc/700/stat":        []byte(statFixture),
			"/proc/800/cmdline":     []byte("node\x00/opt/other/server.js\x00"),
			"/opt/dsh/package.json": []byte(`{"version":"0.2.1-rc.5"}`),
		},
		infos: map[string]os.FileInfo{
			"/opt/dsh/package.json": testRegularFile,
		},
		fences: map[uint16]bool{
			testWebPort: true,
		},
	}

	hosts, err := discoverHosts(context.Background(), DiscoveryConfig{}, fake, "linux")
	if err != nil {
		t.Fatalf("discoverHosts() error = %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("Linux 宿主数 = %d, want 1: %+v", len(hosts), hosts)
	}
	host := hosts[0]
	if host.PID != 700 || host.Port != testWebPort || host.Version != "0.2.1-rc.5" ||
		host.Variant != variantWeb || host.InstallDir != "/opt/dsh" ||
		host.Home != "/home/alice/.dsh" || host.Executable != "node" {
		t.Fatalf("Linux 宿主字段错误: %+v", host)
	}
	if want := testHostGeneration("987654", "node", "node /opt/dsh/lib/bin.js web"); host.Generation != want {
		t.Fatalf("Generation = %q, want %q", host.Generation, want)
	}
	if err := host.Validate(); err != nil {
		t.Fatalf("Linux 宿主未通过校验: %v", err)
	}

	desktop, err := discoverHosts(context.Background(), DiscoveryConfig{Variant: variantDesktop}, fake, "linux")
	if err != nil {
		t.Fatalf("discoverHosts(desktop) error = %v", err)
	}
	if len(desktop) != 0 {
		t.Fatalf("Linux 不支持 Desktop，got %+v", desktop)
	}
}

func TestParseDarwinLsofListeners(t *testing.T) {
	output := "p700\nn127.0.0.1:3080\nn127.0.0.1:3080\nn*:3081\nn192.168.1.2:3082\nn[::1]:3083\nn127.0.0.1:3090\n"
	got := parseDarwinLsofListeners([]byte(output))
	want := []uint16{testWebPort, testDesktopPort}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("parseDarwinLsofListeners() = %v, want %v", got, want)
	}
}

func TestParseLinuxSSListeners(t *testing.T) {
	output := `LISTEN 0 4096 127.0.0.1:3080 0.0.0.0:* users:(("node",pid=700,fd=22))
LISTEN 0 4096 [::1]:3081 [::]:* users:(("node",pid=701,fd=22))
LISTEN 0 4096 0.0.0.0:3082 0.0.0.0:* users:(("node",pid=702,fd=22))
LISTEN 0 4096 127.0.0.1:3090 0.0.0.0:* users:(("node",pid=700,fd=23))
State Recv-Q Send-Q Local Address:Port Peer Address:Port Process
`
	got := parseLinuxSSListeners([]byte(output))
	want := map[uint32][]uint16{
		700: {testWebPort, testDesktopPort},
	}
	if len(got) != len(want) {
		t.Fatalf("parseLinuxSSListeners() = %v, want %v", got, want)
	}
	ports := got[700]
	if len(ports) != 2 || ports[0] != testWebPort || ports[1] != testDesktopPort {
		t.Fatalf("pid 700 端口 = %v, want %v", ports, want[700])
	}
}

func TestParseDarwinProcArgs2Environment(t *testing.T) {
	var buffer bytes.Buffer
	if err := binary.Write(&buffer, binary.LittleEndian, uint32(2)); err != nil {
		t.Fatalf("写入 argc: %v", err)
	}
	buffer.WriteString("/usr/local/bin/node\x00\x00")
	buffer.WriteString("node\x00/opt/dsh/lib/bin.js\x00")
	buffer.WriteString("PATH=/usr/bin\x00DSH_HOME=/Users/alice/.dsh\x00")

	env := parseDarwinProcArgs2(buffer.Bytes())
	if got := dshHomeFromEnvironment(env); got != "/Users/alice/.dsh" {
		t.Fatalf("DSH_HOME = %q, want /Users/alice/.dsh (env=%v)", got, env)
	}
	if len(parseDarwinProcArgs2([]byte{1, 2})) != 0 {
		t.Fatal("过短的 procargs2 数据应返回空环境")
	}
}

func TestSystemProberFenceRequiresAuthenticationBody(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "401 与固定提示", status: http.StatusUnauthorized, body: dshFenceBody, want: true},
		{name: "401 带尾部换行", status: http.StatusUnauthorized, body: dshFenceBody + "\n", want: true},
		{name: "401 但提示不符", status: http.StatusUnauthorized, body: "unauthorized", want: false},
		{name: "200 即使正文相同", status: http.StatusOK, body: dshFenceBody, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/" {
					t.Errorf("fence 请求路径 = %q, want /", request.URL.Path)
				}
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()

			endpoint, err := url.Parse(server.URL)
			if err != nil {
				t.Fatalf("解析测试服务器地址: %v", err)
			}
			port, err := strconv.Atoi(endpoint.Port())
			if err != nil {
				t.Fatalf("解析测试服务器端口: %v", err)
			}
			if got := (systemProber{}).Fence(uint16(port)); got != test.want {
				t.Fatalf("Fence() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDshWebInstallDirResolvesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 需要额外权限创建符号链接")
	}
	root := t.TempDir()
	installDir := filepath.Join(root, "dsh-package")
	libDir := filepath.Join(installDir, "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatalf("创建 lib 目录: %v", err)
	}
	binPath := filepath.Join(libDir, "bin.js")
	if err := os.WriteFile(binPath, []byte("#!/usr/bin/env node\n"), 0o644); err != nil {
		t.Fatalf("写入 bin.js: %v", err)
	}
	linkPath := filepath.Join(root, "dsh")
	if err := os.Symlink(binPath, linkPath); err != nil {
		t.Fatalf("创建符号链接: %v", err)
	}
	resolvedInstall, err := filepath.EvalSymlinks(installDir)
	if err != nil {
		t.Fatalf("解析安装目录: %v", err)
	}

	got, ok := dshWebInstallDir([]string{"node", linkPath, "web"})
	if !ok || got != resolvedInstall {
		t.Fatalf("dshWebInstallDir() = %q, %v; want %q, true", got, ok, resolvedInstall)
	}
	if _, ok := dshWebInstallDir([]string{"node", linkPath, "serve"}); ok {
		t.Fatal("非 web 子命令不应被识别为 dsh web")
	}
}

func TestDshDesktopInstallDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("macOS .app 路径在 Windows 上不是绝对路径")
	}
	args := []string{
		testDesktopInstallDir + "/Contents/MacOS/DeepSeek Harness",
		"--app-path=" + testDesktopInstallDir + "/Contents/Resources/app.asar/node_modules/dsh/desktop-host/index.js",
		"--profile", "profiles/desktop",
	}
	got, ok := dshDesktopInstallDir(args)
	if !ok || got != testDesktopInstallDir {
		t.Fatalf("dshDesktopInstallDir() = %q, %v; want %q, true", got, ok, testDesktopInstallDir)
	}
}
