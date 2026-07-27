package herdr

import (
	"errors"
	"strings"
)

const windowsNamedPipePrefix = `\\.\pipe\`

func windowsNamedPipePath(markerPath string) string {
	return windowsNamedPipePrefix + markerPath
}

func windowsDefaultSocketPath(xdgConfigHome, appData, userProfile, home string) (string, error) {
	if base := strings.TrimSpace(xdgConfigHome); base != "" {
		return joinWindowsPath(base, "herdr", "herdr.sock"), nil
	}
	if base := strings.TrimSpace(appData); base != "" {
		return joinWindowsPath(base, "herdr", "herdr.sock"), nil
	}
	if base := strings.TrimSpace(userProfile); base != "" {
		return joinWindowsPath(base, "AppData", "Roaming", "herdr", "herdr.sock"), nil
	}
	if base := strings.TrimSpace(home); base != "" {
		return joinWindowsPath(base, ".config", "herdr", "herdr.sock"), nil
	}
	return "", errors.New("Windows 配置目录环境变量均为空")
}

func joinWindowsPath(base string, elements ...string) string {
	path := strings.TrimRight(strings.ReplaceAll(base, "/", `\`), `\`)
	for _, element := range elements {
		path += `\` + strings.Trim(strings.ReplaceAll(element, "/", `\`), `\`)
	}
	return path
}
