package installer

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWritePrivateFileBacksUpAndAtomicallyReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	backup, err := writePrivateFile(path, []byte("new"), time.Date(2026, 7, 31, 1, 2, 3, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if backup == "" {
		t.Fatal("backup path is empty")
	}
	assertFileContent(t, path, "new")
	assertFileContent(t, backup, "old")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestWritePrivateFileCreatesPrivateDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")

	backup, err := writePrivateFile(path, []byte("new"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if backup != "" {
		t.Fatalf("backup = %q, want empty", backup)
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("directory mode = %o, want private", info.Mode().Perm())
	}
}

func TestWritePrivateFileRejectsSymbolicLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symbolic link permissions vary")
	}
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	path := filepath.Join(directory, "config.json")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	if _, err := writePrivateFile(path, []byte("replacement"), time.Now()); err == nil {
		t.Fatal("writePrivateFile() should reject symbolic link")
	}
	assertFileContent(t, target, "secret")
}

func TestRestoreBackupRestoresOriginalFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.json")
	backup := filepath.Join(directory, "config.json.bak")
	if err := os.WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := restoreBackup(path, backup); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, path, "old")
}

func TestRestoreBackupRemovesNewFileWhenNoOriginalExisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := restoreBackup(path, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("os.Stat() error = %v, want not exist", err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}

func TestBackupNameDoesNotExposeContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("hpk_sensitive"), 0o600); err != nil {
		t.Fatal(err)
	}
	backup, err := writePrivateFile(path, []byte("new"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(backup, "hpk_sensitive") {
		t.Fatalf("backup path exposes content: %q", backup)
	}
}
