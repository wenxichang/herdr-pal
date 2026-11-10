//go:build darwin

package dsh

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func init() {
	readProcessEnvironment = darwinProcessEnvironment
	discoverDarwinFn = discoverDarwin
}

// discoverDarwin 扫描当前用户的进程，识别 dsh web 与 Desktop，并用 401 fence
// 确认监听端口确实属于 dsh。单个进程读取失败时跳过，不中断整次发现。
func discoverDarwin(ctx context.Context, p Prober) ([]Host, error) {
	processes, err := p.Run(ctx, "ps", "-U", strconv.Itoa(os.Getuid()), "-o", "pid=,uid=,comm=")
	if err != nil {
		return nil, fmt.Errorf("扫描 dsh 进程失败: %w", err)
	}

	var hosts []Host
	for _, process := range parseDarwinProcesses(processes) {
		if !isDarwinDSHCandidate(process.comm) {
			continue
		}
		pidText := strconv.FormatUint(uint64(process.pid), 10)
		argsOutput, err := p.Run(ctx, "ps", "-ww", "-p", pidText, "-o", "args=")
		if err != nil {
			continue
		}
		args := strings.Fields(strings.TrimSpace(string(argsOutput)))
		if len(args) == 0 {
			continue
		}

		variant := ""
		installDir := ""
		if dir, ok := dshWebInstallDir(args); ok {
			variant, installDir = variantWeb, dir
		} else if dir, ok := dshDesktopInstallDir(args); ok {
			variant, installDir = variantDesktop, dir
		}
		if variant == "" {
			continue
		}

		startOutput, err := p.Run(ctx, "ps", "-p", pidText, "-o", "uid=,lstart=")
		if err != nil {
			continue
		}
		started := parsePSStartTime(startOutput)
		if started == "" {
			continue
		}

		version := ""
		if variant == variantWeb {
			version = readWebVersion(p, installDir)
		} else {
			version = readDesktopVersion(ctx, p, installDir)
		}

		home := dshHomeFromEnvironment(readProcessEnvironment(process.pid))
		if home == "" {
			home = defaultDSHHome()
		}

		ports, err := p.LocalListeners(process.pid)
		if err != nil {
			continue
		}

		generation := hostGeneration(started, process.comm, strings.Join(args, " "))
		for _, port := range ports {
			if port == 0 || !p.Fence(port) {
				continue
			}
			hosts = append(hosts, Host{
				PID:        process.pid,
				Port:       port,
				Version:    version,
				Variant:    variant,
				InstallDir: installDir,
				Home:       home,
				Executable: process.comm,
				Generation: generation,
			})
		}
	}
	return hosts, nil
}

type darwinProcess struct {
	pid  uint32
	comm string
}

// parseDarwinProcesses 解析 ps -o pid=,uid=,comm= 输出；comm 可能包含空格。
func parseDarwinProcesses(data []byte) []darwinProcess {
	var processes []darwinProcess
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pidText, rest := cutPSField(line)
		uidText, comm := cutPSField(rest)
		if pidText == "" || uidText == "" {
			continue
		}
		pid, err := strconv.ParseUint(pidText, 10, 32)
		if err != nil {
			continue
		}
		if _, err := strconv.ParseUint(uidText, 10, 32); err != nil {
			continue
		}
		comm = strings.TrimSpace(comm)
		if pid == 0 || comm == "" {
			continue
		}
		processes = append(processes, darwinProcess{pid: uint32(pid), comm: comm})
	}
	return processes
}

func cutPSField(line string) (string, string) {
	line = strings.TrimLeft(line, " \t")
	index := strings.IndexAny(line, " \t")
	if index < 0 {
		return line, ""
	}
	return line[:index], strings.TrimLeft(line[index:], " \t")
}

// parsePSStartTime 去掉 ps uid=,lstart= 输出中的 uid，保留启动时间。
func parsePSStartTime(data []byte) string {
	fields := strings.Fields(strings.TrimSpace(string(data)))
	if len(fields) < 2 {
		return ""
	}
	return strings.Join(fields[1:], " ")
}

func isDarwinDSHCandidate(comm string) bool {
	base := filepath.Base(comm)
	return base == "node" ||
		strings.Contains(comm, "DeepSeek Harness.app") ||
		strings.HasPrefix(base, "DeepSeek Harness")
}

// readDesktopVersion 从 Info.plist 读取 Desktop 版本。
// electron-builder 可能把预发布版本放在 CFBundleVersion，因此两个键都尝试。
func readDesktopVersion(ctx context.Context, p Prober, installDir string) string {
	path := filepath.Join(installDir, "Contents", "Info.plist")
	info, err := p.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	fallback := ""
	for _, key := range []string{"CFBundleShortVersionString", "CFBundleVersion"} {
		output, err := p.Run(ctx, "plutil", "-extract", key, "raw", path)
		if err != nil {
			continue
		}
		version := strings.TrimSpace(string(output))
		if version == "" {
			continue
		}
		if _, supported := supportedVersions[version]; supported {
			return version
		}
		if fallback == "" {
			fallback = version
		}
	}
	return fallback
}

// LocalListeners 通过 lsof 查找目标进程在 127.0.0.1 上的 TCP 监听端口。
func (systemProber) LocalListeners(pid uint32) ([]uint16, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(
		ctx,
		"lsof", "-nP", "-a", "-p", strconv.FormatUint(uint64(pid), 10),
		"-iTCP", "-sTCP:LISTEN", "-Fn",
	).Output()
	if err != nil {
		return nil, err
	}
	return parseDarwinLsofListeners(output), nil
}

// darwinProcessEnvironment 通过 KERN_PROCARGS2 读取目标进程启动时的环境变量。
// 权限不足或格式变化时返回空，发现流程继续使用默认 home。
func darwinProcessEnvironment(pid uint32) []string {
	data, err := unix.SysctlRaw("kern.procargs2", int(pid))
	if err != nil {
		return nil
	}
	return parseDarwinProcArgs2(data)
}
