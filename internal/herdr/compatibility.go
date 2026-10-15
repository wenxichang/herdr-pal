package herdr

import "fmt"

const (
	// MinSupportedProtocol 是 Herdr 公共 Socket API 接受的最低协议版本。
	MinSupportedProtocol uint32 = 17
	// MaxSupportedProtocol 是 Herdr 公共 Socket API 接受的最高协议版本。
	MaxSupportedProtocol uint32 = 22

	// Protocol17 是 Herdr 0.7.5 使用且已完成真实联调的公共 Socket 协议版本。
	Protocol17 uint32 = 17
	// Protocol19 是 Herdr 0.8.0 使用且已完成 Schema 兼容性审计的公共 Socket 协议版本。
	Protocol19 uint32 = 19
)

// IsSupportedProtocol 报告 protocol 是否在接受的 Herdr 公共 Socket API 版本范围内。
func IsSupportedProtocol(protocol uint32) bool {
	return protocol >= MinSupportedProtocol && protocol <= MaxSupportedProtocol
}

// ValidateProtocol 验证 Herdr 版本声明的公共 Socket 协议是否在接受范围内。
func ValidateProtocol(version string, protocol uint32) error {
	if IsSupportedProtocol(protocol) {
		return nil
	}
	return fmt.Errorf("%w: version %s, supported 17 through 22, got %d", ErrProtocolMismatch, version, protocol)
}
