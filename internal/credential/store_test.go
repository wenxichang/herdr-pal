package credential

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStoreIssuePersistsDigestAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	store, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore() error = %v", err)
	}
	store.now = func() time.Time { return time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC) }
	store.random = strings.NewReader(strings.Repeat("r", issueRandomBytes))
	token, record, err := store.Issue("user-a", "office-pc")
	if err != nil {
		t.Fatalf("Store.Issue() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(string(data), token) || strings.Contains(string(data), token[strings.LastIndex(token, "_")+1:]) {
		t.Fatalf("credential file leaked token: %s", data)
	}
	if !strings.Contains(string(data), record.SecretSHA256) {
		t.Fatalf("credential file missing digest: %s", data)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("credential mode = %v, %v", info.Mode().Perm(), err)
		}
	}
	reloaded, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore(reload) error = %v", err)
	}
	reloaded.now = store.now
	identity, err := reloaded.VerifyBearer(context.Background(), token)
	if err != nil || identity.PrincipalID != "user-a" || identity.MachineID != "office-pc" {
		t.Fatalf("VerifyBearer() = %#v, %v", identity, err)
	}
}

func TestLoadStoreRejectsBroadFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不使用 POSIX mode")
	}
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"credentials":[]}`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := LoadStore(path); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("LoadStore() error = %v, want ErrInsecurePermissions", err)
	}
}

func TestStoreRejectsUnknownCredentialWithoutLeakingLookupState(t *testing.T) {
	store, err := LoadStore(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatalf("LoadStore() error = %v", err)
	}
	if _, err := store.VerifyBearer(context.Background(), "hpk_cred-missing_secret"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("VerifyBearer() error = %v, want ErrUnauthenticated", err)
	}
}
