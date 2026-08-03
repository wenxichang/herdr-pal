//go:build windows

package lifecycle

import "os"

type unsupportedSpawner struct{}

// NewDetachedSpawner 在 Windows 上保留编译入口，但当前不启用插件自守护。
func NewDetachedSpawner(*os.File, []string) (SupervisorSpawner, error) {
	return unsupportedSpawner{}, ErrUnsupported
}

func (unsupportedSpawner) Spawn(SupervisorCommand) error { return ErrUnsupported }
