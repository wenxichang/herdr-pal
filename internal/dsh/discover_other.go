//go:build !darwin && !linux

package dsh

import "errors"

// LocalListeners 在非 darwin/linux 平台没有自动发现能力。
// Windows 等平台必须显式配置 dsh.url 与 dsh.home。
func (systemProber) LocalListeners(uint32) ([]uint16, error) {
	return nil, errors.New("当前平台需要显式配置 dsh.url 与 dsh.home")
}
