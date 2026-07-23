package processlock

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestAcquirePreventsSecondProcessUntilReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herdr-pal.lock")

	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("第一次 Acquire() 返回错误：%v", err)
	}

	second, err := Acquire(path)
	if second != nil {
		t.Fatal("第二次 Acquire() 不应返回锁")
	}
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("第二次 Acquire() 错误 = %v，期望 ErrAlreadyRunning", err)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release() 返回错误：%v", err)
	}

	third, err := Acquire(path)
	if err != nil {
		t.Fatalf("释放后 Acquire() 返回错误：%v", err)
	}
	if err := third.Release(); err != nil {
		t.Fatalf("第三次锁 Release() 返回错误：%v", err)
	}
}

func TestAcquireReturnsUnderlyingErrorForUnavailablePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "herdr-pal.lock")

	_, err := Acquire(path)
	if err == nil {
		t.Fatal("Acquire() 未返回路径不可用错误")
	}
	if errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("Acquire() 错误 = %v，不应为 ErrAlreadyRunning", err)
	}
}
