//go:build !windows

package lifecycle

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
)

type detachedSpawner struct {
	output      *os.File
	environment []string
}

// NewDetachedSpawner 创建把 Supervisor 放入独立会话并将输出写入日志文件的启动器。
func NewDetachedSpawner(output *os.File, environment []string) (SupervisorSpawner, error) {
	if output == nil {
		return nil, ErrInvalidLauncher
	}
	return &detachedSpawner{output: output, environment: environment}, nil
}

func (spawner *detachedSpawner) Spawn(command SupervisorCommand) error {
	if strings.TrimSpace(command.Executable) == "" || strings.TrimSpace(command.ConfigPath) == "" || strings.TrimSpace(command.SocketPath) == "" {
		return ErrInvalidLauncher
	}
	child := exec.Command(command.Executable, "__supervise", "-config", command.ConfigPath, "-socket", command.SocketPath)
	environment := spawner.environment
	if environment == nil {
		environment = os.Environ()
	}
	child.Env = replaceProcessEnvironment(environment, "HERDR_SOCKET_PATH", command.SocketPath)
	child.Stdout = spawner.output
	child.Stderr = spawner.output
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		return err
	}
	return child.Process.Release()
}
