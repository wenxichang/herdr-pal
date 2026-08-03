package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenLogFileAppendsBelowLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "herdr-pal.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	file, err := OpenLogFile(path, 1024)
	if err != nil {
		t.Fatalf("OpenLogFile() error = %v", err)
	}
	if _, err := file.WriteString("new\n"); err != nil {
		t.Fatalf("WriteString() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(data) != "old\nnew\n" {
		t.Fatalf("log contents = %q", data)
	}
}

func TestOpenLogFileRotatesAtLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "herdr-pal.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	old := strings.Repeat("x", 32)
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	file, err := OpenLogFile(path, 32)
	if err != nil {
		t.Fatalf("OpenLogFile() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("ReadFile(rotated) error = %v", err)
	}
	if string(rotated) != old {
		t.Fatalf("rotated log contents = %q", rotated)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(log) error = %v", err)
	}
	if info.Size() != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("new log size/mode = %d/%o", info.Size(), info.Mode().Perm())
	}
	directoryInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat(log dir) error = %v", err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("log directory permissions = %o", directoryInfo.Mode().Perm())
	}
}

func TestOpenLogFileRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.log")
	path := filepath.Join(directory, "herdr-pal.log")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	if _, err := OpenLogFile(path, 1024); err == nil {
		t.Fatal("OpenLogFile() should reject symlink")
	}
}
