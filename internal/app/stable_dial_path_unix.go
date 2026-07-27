//go:build !windows

package app

func platformUsesUnixSocketAlias() bool {
	return true
}

func platformResolvesSocketSymlinks() bool {
	return true
}
