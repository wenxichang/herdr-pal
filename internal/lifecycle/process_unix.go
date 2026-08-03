//go:build !windows

package lifecycle

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

type commandWorkerFactory struct {
	options CommandWorkerOptions
}

type commandWorkerProcess struct {
	command *exec.Cmd
}

// NewCommandWorkerFactory 创建使用当前 Pal 二进制内部 Worker 入口的进程工厂。
func NewCommandWorkerFactory(options CommandWorkerOptions) (WorkerFactory, error) {
	options.Executable = strings.TrimSpace(options.Executable)
	options.ConfigPath = strings.TrimSpace(options.ConfigPath)
	options.SocketPath = strings.TrimSpace(options.SocketPath)
	if options.Executable == "" || options.ConfigPath == "" || options.SocketPath == "" {
		return nil, ErrInvalidWorkerSupervisor
	}
	return &commandWorkerFactory{options: options}, nil
}

func (factory *commandWorkerFactory) Start() (WorkerProcess, error) {
	command := exec.Command(factory.options.Executable, "__worker", "-config", factory.options.ConfigPath)
	environment := factory.options.Environment
	if environment == nil {
		environment = os.Environ()
	}
	command.Env = replaceProcessEnvironment(environment, "HERDR_SOCKET_PATH", factory.options.SocketPath)
	command.Stdout = factory.options.Stdout
	command.Stderr = factory.options.Stderr
	if err := command.Start(); err != nil {
		return nil, err
	}
	return &commandWorkerProcess{command: command}, nil
}

func (process *commandWorkerProcess) PID() int {
	if process == nil || process.command == nil || process.command.Process == nil {
		return 0
	}
	return process.command.Process.Pid
}

func (process *commandWorkerProcess) Wait() error {
	if process == nil || process.command == nil {
		return ErrInvalidWorkerSupervisor
	}
	return process.command.Wait()
}

func (process *commandWorkerProcess) Terminate() error {
	if process == nil || process.command == nil || process.command.Process == nil {
		return ErrInvalidWorkerSupervisor
	}
	err := process.command.Process.Signal(syscall.SIGTERM)
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func (process *commandWorkerProcess) Kill() error {
	if process == nil || process.command == nil || process.command.Process == nil {
		return ErrInvalidWorkerSupervisor
	}
	err := process.command.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func replaceProcessEnvironment(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}
