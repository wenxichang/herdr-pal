package dsh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// dshFenceBody 是未认证访问 dsh web 时返回的固定提示；
	// 端口只有同时返回 401 和该正文才认定为 dsh 宿主。
	dshFenceBody = "dsh web authentication required; reopen the URL printed by dsh web."
	// fenceTimeout 限制端口归属探测，避免发现流程被无响应端口阻塞。
	fenceTimeout = 3 * time.Second
)

// DiscoveryConfig 是 dsh 宿主发现配置。
//
// URL 非空时直接构造显式宿主，不扫描本机进程；Home 非空时覆盖发现到的
// DSH_HOME；Variant 为 auto/空时不过滤，否则只保留对应变体。
type DiscoveryConfig struct {
	URL     string
	Home    string
	Variant string
}

// Prober 抽象发现 dsh 宿主所需的系统探测能力。
//
// 生产实现读取真实进程、端口与文件；测试使用固定夹具替换，避免依赖本机
// 正在运行的 dsh。Fence 必须确认端口返回 dsh 的 401 认证提示。
type Prober interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	ReadFile(path string) ([]byte, error)
	Stat(path string) (os.FileInfo, error)
	LocalListeners(pid uint32) ([]uint16, error)
	Fence(port uint16) bool
}

// errExplicitConfigRequired 表示当前平台不支持自动发现，必须显式配置。
var errExplicitConfigRequired = errors.New("当前平台需要显式配置 dsh.url 与 dsh.home")

// readProcessEnvironment 读取目标进程启动时的环境变量。
// 非 darwin 平台返回空；darwin 构建会替换为 KERN_PROCARGS2 实现，测试可直接替换。
var readProcessEnvironment = func(uint32) []string { return nil }

// discoverDarwinFn 由 darwin 构建替换为真实实现；其他平台保持失败关闭。
// 这样 Linux 分支可以在 macOS 上编译并用夹具测试，而 darwin 发现逻辑仍只存在于
// darwin 构建中。
var discoverDarwinFn = func(context.Context, Prober) ([]Host, error) {
	return nil, errExplicitConfigRequired
}

// discoverWithProber 按 goos 分派平台发现实现；未支持的平台要求显式配置。
func discoverWithProber(ctx context.Context, p Prober, goos string) ([]Host, error) {
	switch goos {
	case "darwin":
		return discoverDarwinFn(ctx, p)
	case "linux":
		return discoverLinux(ctx, p)
	default:
		return nil, errExplicitConfigRequired
	}
}

// discoverHosts 返回本机已运行且通过版本门禁的 dsh 宿主。
//
// cfg.URL 非空时只构造一个显式宿主，跳过进程扫描和端口探测；否则按 goos
// 调用对应平台实现。cfg.Home 会覆盖发现到的 home，结果按 Reference 去重并排序。
func discoverHosts(ctx context.Context, cfg DiscoveryConfig, p Prober, goos string) ([]Host, error) {
	if err := validateDiscoveryVariant(cfg.Variant); err != nil {
		return nil, err
	}
	if cfg.URL != "" {
		host, err := explicitHost(cfg)
		if err != nil {
			return nil, err
		}
		return []Host{host}, nil
	}
	hosts, err := discoverWithProber(ctx, p, goos)
	if err != nil {
		return nil, err
	}
	return finalizeHosts(hosts, cfg), nil
}

// explicitHost 由显式 URL 构造宿主；版本兼容性由用户负责，但仍校验端口与变体。
func explicitHost(cfg DiscoveryConfig) (Host, error) {
	endpoint, err := url.Parse(cfg.URL)
	if err != nil || endpoint.Scheme != "http" || endpoint.Hostname() != "127.0.0.1" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		(endpoint.Path != "" && endpoint.Path != "/") {
		return Host{}, errors.New("dsh.url 必须是 http://127.0.0.1:端口，不能包含路径或凭据")
	}
	port, err := parsePort(endpoint.Port())
	if err != nil {
		return Host{}, errors.New("dsh.url 端口必须在 1..65535")
	}

	home := cfg.Home
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return Host{}, errors.New("无法确定用户主目录，请显式配置 dsh.home")
		}
		home = filepath.Join(userHome, ".dsh")
	} else if !filepath.IsAbs(home) {
		return Host{}, errors.New("dsh.home 必须是绝对路径")
	}

	variant := cfg.Variant
	if variant == "" || variant == "auto" {
		variant = variantWeb
	}
	host := Host{
		Port:       port,
		Version:    versionExplicit,
		Variant:    variant,
		Home:       home,
		Generation: hostGeneration("explicit", cfg.URL, home),
	}
	// 显式版本跳过远程版本门禁，但内部字段仍需通过 Host.Validate。
	if err := host.Validate(); err != nil {
		return Host{}, err
	}
	return host, nil
}

// finalizeHosts 应用 home 覆盖与变体过滤，丢弃未知版本，按 Reference 去重排序。
func finalizeHosts(hosts []Host, cfg DiscoveryConfig) []Host {
	seen := make(map[string]struct{}, len(hosts))
	result := make([]Host, 0, len(hosts))
	for _, host := range hosts {
		if cfg.Home != "" {
			host.Home = cfg.Home
		}
		if !variantAllowed(cfg.Variant, host.Variant) {
			continue
		}
		// explicit 是显式配置的内部版本值，自动发现只接受真实 dsh 版本。
		if host.Version == versionExplicit {
			continue
		}
		if err := host.Validate(); err != nil {
			continue
		}
		if _, duplicate := seen[host.Reference()]; duplicate {
			continue
		}
		seen[host.Reference()] = struct{}{}
		result = append(result, host)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Reference() < result[j].Reference()
	})
	return result
}

func validateDiscoveryVariant(variant string) error {
	switch variant {
	case "", "auto", variantWeb, variantDesktop:
		return nil
	default:
		return fmt.Errorf("dsh 变体 %q 不受支持", variant)
	}
}

func variantAllowed(filter, variant string) bool {
	switch filter {
	case "", "auto":
		return true
	case variantWeb, variantDesktop:
		return filter == variant
	default:
		return false
	}
}

// hostGeneration 生成宿主代次，进程重启后变化，使旧目标自动失效。
func hostGeneration(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])[:24]
}

// dshHomeFromEnvironment 从环境变量中提取 DSH_HOME；不存在时返回空字符串。
func dshHomeFromEnvironment(environment []string) string {
	for _, entry := range environment {
		if value, ok := strings.CutPrefix(entry, "DSH_HOME="); ok && value != "" {
			return value
		}
	}
	return ""
}

func defaultDSHHome() string {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(userHome, ".dsh")
}

// dshWebInstallDir 判断参数是否是 `node <script> web` 形式的 dsh web，
// 返回规范化后的安装目录。脚本路径先解析符号链接，兼容 Homebrew 的 dsh 软链。
func dshWebInstallDir(args []string) (string, bool) {
	if len(args) < 2 || !containsArgument(args, "web") {
		return "", false
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") || !strings.Contains(arg, "/") {
			continue
		}
		script := arg
		if resolved, err := filepath.EvalSymlinks(script); err == nil {
			script = resolved
		}
		script = filepath.Clean(script)
		if filepath.Base(script) != "bin.js" || filepath.Base(filepath.Dir(script)) != "lib" {
			continue
		}
		return filepath.Dir(filepath.Dir(script)), true
	}
	return "", false
}

// dshDesktopInstallDir 识别 macOS dsh Desktop 主进程，返回 .app 包路径。
func dshDesktopInstallDir(args []string) (string, bool) {
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "DeepSeek Harness.app") ||
		!strings.Contains(joined, "desktop-host") ||
		!strings.Contains(joined, "index.js") ||
		!strings.Contains(joined, "profiles/desktop") {
		return "", false
	}
	const marker = "DeepSeek Harness.app"
	index := strings.Index(joined, marker)
	start := strings.LastIndexAny(joined[:index], " \t\"'") + 1
	segment := joined[start : index+len(marker)]
	if equals := strings.LastIndex(segment, "="); equals >= 0 {
		segment = segment[equals+1:]
	}
	if !filepath.IsAbs(segment) {
		return "", false
	}
	return filepath.Clean(segment), true
}

func containsArgument(args []string, target string) bool {
	for _, arg := range args {
		if arg == target {
			return true
		}
	}
	return false
}

type packageJSON struct {
	Version string `json:"version"`
}

// readWebVersion 从 web 安装目录的 package.json 读取版本。
// 任何读取或格式异常都返回空字符串，由版本门禁统一过滤。
func readWebVersion(p Prober, installDir string) string {
	path := filepath.Join(installDir, "package.json")
	info, err := p.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	data, err := p.ReadFile(path)
	if err != nil {
		return ""
	}
	var pkg packageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return ""
	}
	return strings.TrimSpace(pkg.Version)
}

// parseDarwinLsofListeners 解析 lsof -Fn 输出，只保留 127.0.0.1 上的监听端口。
func parseDarwinLsofListeners(data []byte) []uint16 {
	var ports []uint16
	seen := make(map[uint16]struct{})
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "n") {
			continue
		}
		host, port, ok := splitLoopbackAddress(line[1:])
		if !ok || host != "127.0.0.1" {
			continue
		}
		if _, duplicate := seen[port]; duplicate {
			continue
		}
		seen[port] = struct{}{}
		ports = append(ports, port)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	return ports
}

// parseDarwinProcArgs2 解析 KERN_PROCARGS2 数据中的环境变量部分。
// 数据格式固定为 argc、可执行路径、argc 个参数，之后是环境变量。
func parseDarwinProcArgs2(data []byte) []string {
	if len(data) < 4 {
		return nil
	}
	argc := int(binary.LittleEndian.Uint32(data[:4]))
	if argc < 0 || argc > 4096 {
		return nil
	}
	rest := data[4:]
	terminator := bytes.IndexByte(rest, 0)
	if terminator < 0 {
		return nil
	}
	rest = bytes.TrimLeft(rest[terminator+1:], "\x00")
	for index := 0; index < argc; index++ {
		terminator = bytes.IndexByte(rest, 0)
		if terminator < 0 {
			return nil
		}
		rest = rest[terminator+1:]
	}
	var environment []string
	for _, entry := range bytes.Split(rest, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		environment = append(environment, string(entry))
	}
	return environment
}

func splitLoopbackAddress(address string) (string, uint16, bool) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return "", 0, false
	}
	port, err := parsePort(portText)
	if err != nil {
		return "", 0, false
	}
	return host, port, true
}

func parsePort(raw string) (uint16, error) {
	if raw == "" {
		return 0, errors.New("端口为空")
	}
	for _, char := range raw {
		if char < '0' || char > '9' {
			return 0, errors.New("端口必须是十进制数字")
		}
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > 65535 {
		return 0, errors.New("端口必须在 1..65535")
	}
	return uint16(value), nil
}

// systemProber 是生产环境使用的 Prober 实现。
type systemProber struct{}

func (systemProber) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

func (systemProber) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (systemProber) Stat(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

// Fence 确认 port 属于 dsh：GET / 必须返回 401 和固定提示正文。
// 使用独立客户端禁用代理与重定向，避免把本机探测发往其他网络目标。
func (systemProber) Fence(port uint16) bool {
	client := &http.Client{
		Timeout: fenceTimeout,
		Transport: &http.Transport{
			Proxy:             nil,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(body)) == dshFenceBody
}
