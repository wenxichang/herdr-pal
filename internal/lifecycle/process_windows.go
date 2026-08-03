//go:build windows

package lifecycle

// NewCommandWorkerFactory 在 Windows 上保留编译入口，但当前不启用自动守护。
func NewCommandWorkerFactory(CommandWorkerOptions) (WorkerFactory, error) {
	return nil, ErrUnsupported
}
