//go:build windows

package app

func platformUsesUnixSocketAlias() bool {
	return false
}

func platformResolvesSocketSymlinks() bool {
	return false
}
