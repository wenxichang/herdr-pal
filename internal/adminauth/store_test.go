package adminauth

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStoreBootstrapPersistsOnlyDigestsAndReturnsSecretsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "server-auth.json")
	store, bootstrap, err := Load(path, deterministicStoreOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !bootstrap.Created || bootstrap.Username != "admin" || bootstrap.InitialPassword == "" || bootstrap.AutomationToken == "" {
		t.Fatalf("bootstrap = %#v", bootstrap)
	}
	admin, err := store.Authenticate("ADMIN", bootstrap.InitialPassword)
	if err != nil || admin.Username != "admin" || !admin.MustChangePassword {
		t.Fatalf("Authenticate() = %#v, %v", admin, err)
	}
	identity, err := store.VerifyAutomationBearer(bootstrap.AutomationToken)
	if err != nil || identity.Username != "admin" {
		t.Fatalf("VerifyAutomationBearer() = %#v, %v", identity, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{bootstrap.InitialPassword, bootstrap.AutomationToken} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("auth file leaked bootstrap secret: %s", data)
		}
	}
	for _, required := range []string{"password_hash", "secret_sha256", "token_id"} {
		if !strings.Contains(string(data), required) {
			t.Fatalf("auth file missing %q: %s", required, data)
		}
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("auth file mode = %v, %v", info.Mode().Perm(), err)
		}
		dirInfo, err := os.Stat(filepath.Dir(path))
		if err != nil || dirInfo.Mode().Perm() != 0o700 {
			t.Fatalf("auth directory mode = %v, %v", dirInfo.Mode().Perm(), err)
		}
	}
	_, reloadedBootstrap, err := Load(path, deterministicStoreOptions())
	if err != nil {
		t.Fatal(err)
	}
	if reloadedBootstrap != (Bootstrap{}) {
		t.Fatalf("reloaded bootstrap = %#v", reloadedBootstrap)
	}
}

func TestStoreAdminLifecycleAndLastAdminProtection(t *testing.T) {
	store, bootstrap, err := Load(filepath.Join(t.TempDir(), "server-auth.json"), deterministicStoreOptions())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateAdmin("Ops.Team")
	if err != nil {
		t.Fatal(err)
	}
	if created.Admin.Username != "ops.team" || created.InitialPassword == "" || created.AutomationToken == "" || !created.Admin.MustChangePassword {
		t.Fatalf("created = %#v", created)
	}
	if _, err := store.VerifyAutomationBearer(created.AutomationToken); err != nil {
		t.Fatalf("new admin token before password change: %v", err)
	}
	if _, err := store.CreateAdmin("ops.team"); !errors.Is(err, ErrAdminExists) {
		t.Fatalf("duplicate CreateAdmin() error = %v", err)
	}
	if err := store.ChangePassword("ops.team", "wrong password", "replacement password"); !errors.Is(err, ErrAuthenticationFailed) {
		t.Fatalf("ChangePassword(wrong) error = %v", err)
	}
	if err := store.ChangePassword("ops.team", created.InitialPassword, "replacement password"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.VerifyAutomationBearer(created.AutomationToken); err != nil {
		t.Fatalf("new admin token after password change: %v", err)
	}
	changed, err := store.Authenticate("ops.team", "replacement password")
	if err != nil || changed.MustChangePassword {
		t.Fatalf("changed admin = %#v, %v", changed, err)
	}
	oldToken := created.AutomationToken
	resetPassword, err := store.ResetPassword("ops.team")
	if err != nil {
		t.Fatal(err)
	}
	if resetPassword == "" {
		t.Fatal("ResetPassword returned empty password")
	}
	if _, err := store.VerifyAutomationBearer(oldToken); err != nil {
		t.Fatalf("password reset rotated token: %v", err)
	}
	resetAdmin, err := store.Authenticate("ops.team", resetPassword)
	if err != nil || !resetAdmin.MustChangePassword {
		t.Fatalf("reset admin = %#v, %v", resetAdmin, err)
	}
	if err := store.DeleteAdmin("admin", "ops.team"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.VerifyAutomationBearer(oldToken); !errors.Is(err, ErrAuthenticationFailed) {
		t.Fatalf("deleted admin token error = %v", err)
	}
	if err := store.DeleteAdmin("admin", "admin"); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("DeleteAdmin(last) error = %v", err)
	}
	if _, err := store.Authenticate("admin", bootstrap.InitialPassword); err != nil {
		t.Fatalf("last admin was deleted: %v", err)
	}
}

func TestStoreAutomationTokenRotationAndEnableState(t *testing.T) {
	store, bootstrap, err := Load(filepath.Join(t.TempDir(), "server-auth.json"), deterministicStoreOptions())
	if err != nil {
		t.Fatal(err)
	}
	rotated, view, err := store.RotateAutomationToken("admin")
	if err != nil {
		t.Fatal(err)
	}
	if rotated == bootstrap.AutomationToken || !view.Enabled || view.TokenID == "" {
		t.Fatalf("rotated=%q view=%#v", rotated, view)
	}
	if _, err := store.VerifyAutomationBearer(bootstrap.AutomationToken); !errors.Is(err, ErrAuthenticationFailed) {
		t.Fatalf("old token error = %v", err)
	}
	if _, err := store.VerifyAutomationBearer(rotated); err != nil {
		t.Fatal(err)
	}
	disabled, err := store.SetAutomationTokenEnabled("admin", false)
	if err != nil || disabled.Enabled {
		t.Fatalf("disabled = %#v, %v", disabled, err)
	}
	if _, err := store.VerifyAutomationBearer(rotated); !errors.Is(err, ErrAuthenticationFailed) {
		t.Fatalf("disabled token error = %v", err)
	}
	if _, err := store.SetAutomationTokenEnabled("admin", true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.VerifyAutomationBearer(rotated); err != nil {
		t.Fatalf("re-enabled token error = %v", err)
	}
}

func TestStoreRetriesAutomationTokenIDCollision(t *testing.T) {
	store, _, err := Load(filepath.Join(t.TempDir(), "server-auth.json"), deterministicStoreOptions())
	if err != nil {
		t.Fatal(err)
	}
	existing, err := hex.DecodeString(store.admins["admin"].AutomationToken.TokenID)
	if err != nil {
		t.Fatal(err)
	}
	randomData := make([]byte, 0, 40+40+40)
	randomData = append(randomData, bytes.Repeat([]byte{0x11}, 40)...)
	randomData = append(randomData, existing...)
	randomData = append(randomData, bytes.Repeat([]byte{0x22}, automationSecretBytes)...)
	randomData = append(randomData, bytes.Repeat([]byte{0x33}, automationTokenIDBytes)...)
	randomData = append(randomData, bytes.Repeat([]byte{0x44}, automationSecretBytes)...)
	store.random = bytes.NewReader(randomData)
	created, err := store.CreateAdmin("operator")
	if err != nil {
		t.Fatal(err)
	}
	if created.Admin.AutomationToken.TokenID == store.admins["admin"].AutomationToken.TokenID {
		t.Fatalf("token collision was not retried: %#v", created.Admin.AutomationToken)
	}
}

func TestStoreRejectsInsecureFileOrDirectoryPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不使用 POSIX 权限位")
	}
	path := filepath.Join(t.TempDir(), "state", "server-auth.json")
	if _, _, err := Load(path, deterministicStoreOptions()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path, deterministicStoreOptions()); !errors.Is(err, ErrInsecureAuthPermissions) {
		t.Fatalf("Load(insecure file) error = %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path, deterministicStoreOptions()); !errors.Is(err, ErrInsecureAuthPermissions) {
		t.Fatalf("Load(insecure directory) error = %v", err)
	}
}

func TestStoreRejectsInvalidUsernamesCorruptFilesAndUnknownVersions(t *testing.T) {
	store, _, err := Load(filepath.Join(t.TempDir(), "valid", "server-auth.json"), deterministicStoreOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, username := range []string{"ab", "1admin", "bad user", strings.Repeat("a", 33)} {
		if _, err := store.CreateAdmin(username); !errors.Is(err, ErrInvalidUsername) {
			t.Fatalf("CreateAdmin(%q) error = %v", username, err)
		}
	}
	for _, content := range []string{`{`, `{"version":2,"admins":[]}`, `{"version":1,"admins":[],"extra":true}`} {
		path := filepath.Join(t.TempDir(), "server-auth.json")
		if runtime.GOOS != "windows" {
			if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Load(path, deterministicStoreOptions()); !errors.Is(err, ErrInvalidAuthFile) {
			t.Fatalf("Load(%q) error = %v", content, err)
		}
	}
}

func TestStorePersistenceFailureDoesNotChangeMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server-auth.json")
	store, bootstrap, err := Load(path, deterministicStoreOptions())
	if err != nil {
		t.Fatal(err)
	}
	store.path = t.TempDir()
	if err := store.ChangePassword("admin", bootstrap.InitialPassword, "replacement password"); err == nil {
		t.Fatal("ChangePassword should fail when destination is a directory")
	}
	if _, err := store.Authenticate("admin", bootstrap.InitialPassword); err != nil {
		t.Fatalf("memory changed after persistence failure: %v", err)
	}
}

func deterministicStoreOptions() Options {
	return Options{
		Now:    func() time.Time { return time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC) },
		Random: &incrementingReader{next: 0x42},
	}
}

type incrementingReader struct {
	next byte
}

func (reader *incrementingReader) Read(value []byte) (int, error) {
	for index := range value {
		value[index] = reader.next
		reader.next++
	}
	return len(value), nil
}
