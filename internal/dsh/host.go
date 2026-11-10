package dsh

import (
	"errors"
	"fmt"
)

const (
	variantWeb     = "web"
	variantDesktop = "desktop"
	// versionExplicit 是显式配置宿主的内部版本值；此时版本兼容性由用户负责。
	versionExplicit = "explicit"
)

// supportedVersions 是经过验证的 dsh 版本白名单，未知版本一律失败关闭。
var supportedVersions = map[string]struct{}{
	"0.2.1-rc.5":    {},
	"0.2.2-rc.1":    {},
	versionExplicit: {},
}

// Host 描述一个可连接的 dsh 宿主身份。
//
// Generation 在 dsh 进程重启后变化，使旧宿主引用和旧目标自动失效；
// PID 为 0 表示没有可发现进程的显式配置宿主。
type Host struct {
	PID        uint32
	Port       uint16
	Version    string // 0.2.1-rc.5、0.2.2-rc.1 或 explicit
	Variant    string // web | desktop
	InstallDir string
	Home       string // 含 .credentials.yaml
	Executable string
	Generation string // sha256(启动时间|可执行文件|参数)[:24]
}

// Reference 返回宿主稳定身份，格式为 harness:<pid>:<port>:<generation>。
// 进程重启后 Generation 变化，引用随之失效。
func (h Host) Reference() string {
	return fmt.Sprintf("harness:%d:%d:%s", h.PID, h.Port, h.Generation)
}

// URL 返回宿主 Remote API 的本机回环根地址。
func (h Host) URL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/", h.Port)
}

// Validate 校验宿主是否满足当前版本门禁与连接前提。
//
// 只接受白名单版本（含显式配置的 explicit）、1..65535 端口和 web/desktop 变体；
// PID 允许为 0，供显式配置使用。任何未知取值都返回错误，由调用方跳过该宿主。
func (h Host) Validate() error {
	if _, ok := supportedVersions[h.Version]; !ok {
		return fmt.Errorf("dsh 版本 %q 不受支持", h.Version)
	}
	if h.Port == 0 {
		return errors.New("dsh 端口必须在 1..65535")
	}
	if h.Variant != variantWeb && h.Variant != variantDesktop {
		return fmt.Errorf("dsh 变体 %q 不受支持", h.Variant)
	}
	return nil
}
