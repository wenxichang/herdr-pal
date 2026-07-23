// Package processlock 负责保证 Herdr Pal 同时只运行一个进程。
package processlock

import (
	"errors"
	"fmt"

	"github.com/gofrs/flock"
)

// ErrAlreadyRunning 表示已有 Herdr Pal 进程持有锁。
var ErrAlreadyRunning = errors.New("herdr-pal 已在运行")

// Lock 表示一个已获取的进程锁。
type Lock struct {
	file *flock.Flock
}

// Acquire 使用 path 对应的文件获取非阻塞进程锁。
func Acquire(path string) (*Lock, error) {
	file := flock.New(path)
	locked, err := file.TryLock()
	if err != nil {
		return nil, fmt.Errorf("获取进程锁: %w", err)
	}
	if !locked {
		return nil, ErrAlreadyRunning
	}
	return &Lock{file: file}, nil
}

// Release 释放当前进程锁。
func (l *Lock) Release() error {
	if err := l.file.Unlock(); err != nil {
		return fmt.Errorf("释放进程锁: %w", err)
	}
	return nil
}
