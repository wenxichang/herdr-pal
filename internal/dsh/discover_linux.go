//go:build linux

// Linux 的 /proc 与 ss 解析位于 discover_proc.go：该文件不使用平台构建标签，
// 以便在 macOS 等宿主上用 fake Prober 覆盖 Linux 分支。
// Go 会按文件名把 *_linux.go 限定为 Linux 构建，因此可跨平台测试的代码不能放在这里。

package dsh

import (
	"context"
	"os/exec"
	"time"
)

// LocalListeners 在 Linux 上通过 ss 查找目标进程的回环监听端口。
// 发现主流程直接解析 ss 输出以同时得到 pid 和端口；该方法保留完整的
// Prober 实现，供单进程查询使用。
func (systemProber) LocalListeners(pid uint32) ([]uint16, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "ss", "-H", "-ltnp").Output()
	if err != nil {
		return nil, err
	}
	return parseLinuxSSListeners(output)[pid], nil
}
