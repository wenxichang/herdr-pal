package herdr

import "fmt"

const (
	// Protocol17 是 Herdr 0.7.5 使用且已完成真实联调的公共 Socket 协议版本。
	Protocol17 uint32 = 17
	// Protocol19 是 Herdr 0.8.0 使用且已完成 Schema 兼容性审计的公共 Socket 协议版本。
	Protocol19 uint32 = 19
)

// IsSupportedProtocol 报告 protocol 是否属于已审计的 Herdr 公共 Socket 协议版本。
func IsSupportedProtocol(protocol uint32) bool {
	switch protocol {
	case Protocol17, Protocol19:
		return true
	default:
		return false
	}
}

// ValidateProtocol 验证 Herdr 版本声明的公共 Socket 协议是否已完成兼容性审计。
func ValidateProtocol(version string, protocol uint32) error {
	if IsSupportedProtocol(protocol) {
		return nil
	}
	return fmt.Errorf("%w: version %s, supported 17 or 19, got %d", ErrProtocolMismatch, version, protocol)
}
