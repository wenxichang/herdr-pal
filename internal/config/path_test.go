package config

import (
	"path/filepath"
	"testing"
)

func TestDefaultClientAndServerRuntimePathsUseSeparateDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	clientPath, err := DefaultClientPath()
	if err != nil {
		t.Fatal(err)
	}
	serverPath, err := DefaultServerPath()
	if err != nil {
		t.Fatal(err)
	}
	authPath, err := DefaultServerAuthPath()
	if err != nil {
		t.Fatal(err)
	}
	bootstrapPath, err := DefaultServerBootstrapPath()
	if err != nil {
		t.Fatal(err)
	}
	helpPath, err := DefaultServerHelpPath()
	if err != nil {
		t.Fatal(err)
	}

	if clientPath != filepath.Join(home, ".config", "herdr-pal", "config.json") {
		t.Fatalf("client path = %q", clientPath)
	}
	serverDirectory := filepath.Join(home, ".config", "herdr-pal-server")
	wants := map[string]string{
		"server":    filepath.Join(serverDirectory, "server.json"),
		"auth":      filepath.Join(serverDirectory, "auth.json"),
		"bootstrap": filepath.Join(serverDirectory, "bootstrap.txt"),
		"help":      filepath.Join(serverDirectory, "help.md"),
	}
	got := map[string]string{"server": serverPath, "auth": authPath, "bootstrap": bootstrapPath, "help": helpPath}
	for name, want := range wants {
		if got[name] != want {
			t.Errorf("%s path = %q, want %q", name, got[name], want)
		}
	}
}
