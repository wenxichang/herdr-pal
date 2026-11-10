package dsh

// 本文件实现 Linux 的 /proc 与 ss 解析，供 discover_linux.go 的平台探测使用。
// 这里不使用平台构建标签，使 macOS 等宿主也能用 fake Prober 验证 Linux 发现逻辑。

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// discoverLinux 通过 ss 枚举本机回环监听，再读取 /proc/<pid> 判断是否为
// dsh web。Linux 只支持 web，Desktop 不在支持范围。
func discoverLinux(ctx context.Context, p Prober) ([]Host, error) {
	output, err := p.Run(ctx, "ss", "-H", "-ltnp")
	if err != nil {
		return nil, fmt.Errorf("扫描 dsh 监听失败: %w", err)
	}
	listeners := parseLinuxSSListeners(output)
	pids := make([]uint32, 0, len(listeners))
	for pid := range listeners {
		pids = append(pids, pid)
	}
	sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })

	var hosts []Host
	for _, pid := range pids {
		pidText := strconv.FormatUint(uint64(pid), 10)
		cmdline, err := p.ReadFile("/proc/" + pidText + "/cmdline")
		if err != nil || len(cmdline) == 0 {
			continue
		}
		args := splitNulFields(cmdline)
		if len(args) == 0 {
			continue
		}
		installDir, ok := dshWebInstallDir(args)
		if !ok {
			continue
		}

		home := ""
		if environment, err := p.ReadFile("/proc/" + pidText + "/environ"); err == nil {
			home = dshHomeFromEnvironment(splitNulFields(environment))
		}
		if home == "" {
			home = defaultDSHHome()
		}

		started := ""
		if processStat, err := p.ReadFile("/proc/" + pidText + "/stat"); err == nil {
			started = parseLinuxProcessStartTime(processStat)
		}

		generation := hostGeneration(started, args[0], strings.Join(args, " "))
		version := readWebVersion(p, installDir)
		for _, port := range listeners[pid] {
			if port == 0 || !p.Fence(port) {
				continue
			}
			hosts = append(hosts, Host{
				PID:        pid,
				Port:       port,
				Version:    version,
				Variant:    variantWeb,
				InstallDir: installDir,
				Home:       home,
				Executable: args[0],
				Generation: generation,
			})
		}
	}
	return hosts, nil
}

// parseLinuxSSListeners 解析 ss -H -ltnp 输出，只保留 127.0.0.1 上的监听，
// 返回 pid 到端口的有序映射；同一 pid 的重复端口会被去掉。
func parseLinuxSSListeners(data []byte) map[uint32][]uint16 {
	listeners := make(map[uint32][]uint16)
	seen := make(map[uint32]map[uint16]struct{})
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != "LISTEN" {
			continue
		}
		host, port, ok := splitLoopbackAddress(fields[3])
		if !ok || host != "127.0.0.1" {
			continue
		}
		for _, pid := range ssLinePIDs(line) {
			if seen[pid] == nil {
				seen[pid] = make(map[uint16]struct{})
			}
			if _, duplicate := seen[pid][port]; duplicate {
				continue
			}
			seen[pid][port] = struct{}{}
			listeners[pid] = append(listeners[pid], port)
		}
	}
	for pid := range listeners {
		ports := listeners[pid]
		sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
		listeners[pid] = ports
	}
	return listeners
}

// ssLinePIDs 提取一行 ss 输出中 users:(("node",pid=123,fd=4)) 的进程号。
func ssLinePIDs(line string) []uint32 {
	var pids []uint32
	seen := make(map[uint32]struct{})
	rest := line
	for {
		index := strings.Index(rest, "pid=")
		if index < 0 {
			break
		}
		rest = rest[index+len("pid="):]
		end := 0
		for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
			end++
		}
		if end == 0 {
			continue
		}
		value, err := strconv.ParseUint(rest[:end], 10, 32)
		if err == nil && value > 0 {
			pid := uint32(value)
			if _, duplicate := seen[pid]; !duplicate {
				seen[pid] = struct{}{}
				pids = append(pids, pid)
			}
		}
		rest = rest[end:]
	}
	return pids
}

// parseLinuxProcessStartTime 从 /proc/<pid>/stat 读取第 22 个字段 starttime，
// 用于保证宿主代次在进程重启后变化。
func parseLinuxProcessStartTime(data []byte) string {
	closeIndex := bytes.LastIndexByte(data, ')')
	if closeIndex < 0 {
		return ""
	}
	fields := strings.Fields(string(data[closeIndex+1:]))
	// 右括号后第一个字段是总第 3 个字段 state，starttime 是总第 22 个字段。
	const startTimeOffset = 22 - 3
	if len(fields) <= startTimeOffset {
		return ""
	}
	return fields[startTimeOffset]
}

func splitNulFields(data []byte) []string {
	parts := bytes.Split(data, []byte{0})
	fields := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		fields = append(fields, string(part))
	}
	return fields
}
