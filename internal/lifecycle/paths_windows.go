//go:build windows

package lifecycle

// DefaultRuntimePaths 在 Windows 上保留编译入口，但当前不启用插件自守护。
func DefaultRuntimePaths(string) (RuntimePaths, error) {
	return RuntimePaths{}, ErrUnsupported
}
