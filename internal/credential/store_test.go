package credential

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var storeTestNow = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

func TestStoreIssuePersistsDigestSourcesAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "credentials.json")
	store := loadStoreForTest(t, path)
	token, record, err := store.Issue("user-a", "office-pc", []string{"192.168.1.42/24"}, nil)
	if err != nil {
		t.Fatalf("Store.Issue() error = %v", err)
	}
	if record.CredentialID != 1 || record.AllowedSources[0] != "192.168.1.0/24" {
		t.Fatalf("record = %#v", record)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := parseToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) || strings.Contains(string(data), secret) {
		t.Fatalf("credential file leaked token: %s", data)
	}
	if !strings.Contains(string(data), record.SecretSHA256) || !strings.Contains(string(data), `"allowed_sources"`) {
		t.Fatalf("credential file missing record data: %s", data)
	}
	if runtime.GOOS != "windows" {
		fileInfo, err := os.Stat(path)
		if err != nil || fileInfo.Mode().Perm() != 0o600 {
			t.Fatalf("credential mode = %v, %v", fileInfo.Mode().Perm(), err)
		}
		dirInfo, err := os.Stat(filepath.Dir(path))
		if err != nil || dirInfo.Mode().Perm() != 0o700 {
			t.Fatalf("credential directory mode = %v, %v", dirInfo.Mode().Perm(), err)
		}
	}
	reloaded := loadStoreForTest(t, path)
	identity, err := reloaded.VerifyBearer(context.Background(), token, netip.MustParseAddr("192.168.1.20"))
	if err != nil || identity.CredentialID != 1 || identity.PrincipalID != "user-a" || identity.MachineID != "office-pc" {
		t.Fatalf("VerifyBearer() = %#v, %v", identity, err)
	}
	if _, err := reloaded.VerifyBearer(context.Background(), token, netip.MustParseAddr("10.0.0.1")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("VerifyBearer(source mismatch) error = %v", err)
	}
}

func TestStoreAllocatesMaxPlusOneAndOnlyReusesDeletedTailAfterReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	store := loadStoreForTest(t, path)
	first := issueStoreRecord(t, store, "first")
	second := issueStoreRecord(t, store, "second")
	third := issueStoreRecord(t, store, "third")
	if first.CredentialID != 1 || second.CredentialID != 2 || third.CredentialID != 3 {
		t.Fatalf("issued ids = %d, %d, %d", first.CredentialID, second.CredentialID, third.CredentialID)
	}
	if _, err := store.Delete(2); err != nil {
		t.Fatal(err)
	}
	fourth := issueStoreRecord(t, store, "fourth")
	if fourth.CredentialID != 4 {
		t.Fatalf("runtime id after middle delete = %d, want 4", fourth.CredentialID)
	}
	if _, err := store.Delete(4); err != nil {
		t.Fatal(err)
	}
	reloaded := loadStoreForTest(t, path)
	afterReload := issueStoreRecord(t, reloaded, "after-reload")
	if afterReload.CredentialID != 4 {
		t.Fatalf("id after deleted tail reload = %d, want 4", afterReload.CredentialID)
	}
}

func TestStoreRejectsCredentialIDOverflow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	record := issueRecordForStoreTest(t, math.MaxUint64, "last")
	writeStoreFile(t, path, []Record{record})
	store := loadStoreForTest(t, path)
	if _, _, err := store.Issue("user", "next", []string{"127.0.0.1"}, nil); !errors.Is(err, ErrCredentialIDExhausted) {
		t.Fatalf("Store.Issue() error = %v, want ErrCredentialIDExhausted", err)
	}
}

func TestStoreConcurrentIssueAllocatesUniqueIDs(t *testing.T) {
	store := loadStoreForTest(t, filepath.Join(t.TempDir(), "credentials.json"))
	const count = 24
	ids := make(chan uint64, count)
	errs := make(chan error, count)
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, record, err := store.Issue("user", "machine-"+formatStoreTestIndex(index), []string{"127.0.0.1"}, nil)
			if err != nil {
				errs <- err
				return
			}
			ids <- record.CredentialID
		}(index)
	}
	wait.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	seen := make(map[uint64]struct{}, count)
	for id := range ids {
		seen[id] = struct{}{}
	}
	if len(seen) != count {
		t.Fatalf("unique ids = %d, want %d", len(seen), count)
	}
}

func TestStoreRejectsDuplicatePrincipalAndMachineWithoutConsumingID(t *testing.T) {
	store := loadStoreForTest(t, filepath.Join(t.TempDir(), "credentials.json"))
	first := issueStoreRecord(t, store, "home")
	if _, _, err := store.Issue("user", "home", []string{"10.0.0.1"}, nil); !errors.Is(err, ErrCredentialConflict) {
		t.Fatalf("Store.Issue(duplicate) error = %v, want ErrCredentialConflict", err)
	}
	second := issueStoreRecord(t, store, "office")
	if first.CredentialID != 1 || second.CredentialID != 2 {
		t.Fatalf("credential ids = %d, %d", first.CredentialID, second.CredentialID)
	}
	if records := store.List(); len(records) != 2 {
		t.Fatalf("records = %#v", records)
	}
}

func TestStoreListShowAndStatusCRUDReturnIndependentRecords(t *testing.T) {
	store := loadStoreForTest(t, filepath.Join(t.TempDir(), "credentials.json"))
	first := issueStoreRecord(t, store, "first")
	second := issueStoreRecord(t, store, "second")
	list := store.List()
	if len(list) != 2 || list[0].CredentialID != first.CredentialID || list[1].CredentialID != second.CredentialID {
		t.Fatalf("List() = %#v", list)
	}
	list[0].AllowedSources[0] = "10.0.0.1"
	shown, err := store.Show(first.CredentialID)
	if err != nil || shown.AllowedSources[0] != "127.0.0.1" {
		t.Fatalf("Show() = %#v, %v", shown, err)
	}
	disabled, err := store.Disable(first.CredentialID)
	if err != nil || disabled.Status != StatusDisabled || !disabled.UpdatedAt.Equal(storeTestNow) {
		t.Fatalf("Disable() = %#v, %v", disabled, err)
	}
	enabled, err := store.Enable(first.CredentialID)
	if err != nil || enabled.Status != StatusEnabled {
		t.Fatalf("Enable() = %#v, %v", enabled, err)
	}
	deleted, err := store.Delete(second.CredentialID)
	if err != nil || deleted.CredentialID != second.CredentialID {
		t.Fatalf("Delete() = %#v, %v", deleted, err)
	}
	if _, err := store.Show(second.CredentialID); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("Show(deleted) error = %v", err)
	}
}

func TestStoreSourceCRUDNormalizesAndPreventsEmptyPolicy(t *testing.T) {
	store := loadStoreForTest(t, filepath.Join(t.TempDir(), "credentials.json"))
	record := issueStoreRecord(t, store, "machine")
	updated, err := store.AddSources(record.CredentialID, []string{"192.168.1.42/24", "127.0.0.1"})
	if err != nil || !reflect.DeepEqual(updated.AllowedSources, []SourceRule{"127.0.0.1", "192.168.1.0/24"}) {
		t.Fatalf("AddSources() = %#v, %v", updated, err)
	}
	updated, err = store.RemoveSources(record.CredentialID, []string{"127.0.0.1"})
	if err != nil || !reflect.DeepEqual(updated.AllowedSources, []SourceRule{"192.168.1.0/24"}) {
		t.Fatalf("RemoveSources() = %#v, %v", updated, err)
	}
	if _, err := store.RemoveSources(record.CredentialID, []string{"192.168.1.0/24"}); !errors.Is(err, ErrSourceRequired) {
		t.Fatalf("RemoveSources(last) error = %v", err)
	}
	updated, err = store.SetSources(record.CredentialID, []string{"2001:db8::1-2001:db8::5"})
	if err != nil || !reflect.DeepEqual(updated.AllowedSources, []SourceRule{"2001:db8::1-2001:db8::5"}) {
		t.Fatalf("SetSources() = %#v, %v", updated, err)
	}
}

func TestStorePersistenceFailureRollsBackMemory(t *testing.T) {
	store := loadStoreForTest(t, filepath.Join(t.TempDir(), "credentials.json"))
	record := issueStoreRecord(t, store, "machine")
	store.path = t.TempDir()
	if _, err := store.Disable(record.CredentialID); err == nil {
		t.Fatal("Disable() should fail when destination is a directory")
	}
	shown, err := store.Show(record.CredentialID)
	if err != nil || shown.Status != StatusEnabled {
		t.Fatalf("Show() after rollback = %#v, %v", shown, err)
	}
}

func TestStorePostRenameDirectorySyncFailureKeepsDiskAndMemoryCommitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	store := loadStoreForTest(t, path)
	record := issueStoreRecord(t, store, "machine")
	store.syncDirectory = func(string) error { return errors.New("directory sync unavailable") }

	disabled, err := store.Disable(record.CredentialID)
	if err != nil || disabled.Status != StatusDisabled {
		t.Fatalf("Disable() = %#v, %v", disabled, err)
	}
	shown, err := store.Show(record.CredentialID)
	if err != nil || shown.Status != StatusDisabled {
		t.Fatalf("Show() after committed rename = %#v, %v", shown, err)
	}
	reloaded := loadStoreForTest(t, path)
	reloadedRecord, err := reloaded.Show(record.CredentialID)
	if err != nil || reloadedRecord.Status != StatusDisabled {
		t.Fatalf("reloaded record after committed rename = %#v, %v", reloadedRecord, err)
	}
}

func TestLoadStoreRejectsBroadPermissionsOldFormatAndDuplicateID(t *testing.T) {
	if runtime.GOOS != "windows" {
		path := filepath.Join(t.TempDir(), "credentials.json")
		if err := os.WriteFile(path, []byte(`{"version":2,"credentials":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadStore(path); !errors.Is(err, ErrInsecurePermissions) {
			t.Fatalf("LoadStore(broad mode) error = %v", err)
		}
	}
	oldPath := filepath.Join(t.TempDir(), "old.json")
	if err := os.WriteFile(oldPath, []byte(`{"version":1,"credentials":[{"credential_id":"cred-old"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadStore(oldPath); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("LoadStore(old format) error = %v", err)
	}
	record := issueRecordForStoreTest(t, 1, "machine")
	duplicatePath := filepath.Join(t.TempDir(), "duplicate.json")
	writeStoreFile(t, duplicatePath, []Record{record, record})
	if _, err := LoadStore(duplicatePath); !errors.Is(err, ErrCredentialConflict) {
		t.Fatalf("LoadStore(duplicate) error = %v", err)
	}
}

func TestStoreRejectsUnknownCredentialWithoutLeakingLookupState(t *testing.T) {
	store := loadStoreForTest(t, filepath.Join(t.TempDir(), "credentials.json"))
	if _, err := store.VerifyBearer(context.Background(), "hpk_999_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", netip.MustParseAddr("127.0.0.1")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("VerifyBearer() error = %v, want ErrUnauthenticated", err)
	}
	for _, operation := range []func() error{
		func() error { _, err := store.Show(999); return err },
		func() error { _, err := store.Enable(999); return err },
		func() error { _, err := store.Disable(999); return err },
		func() error { _, err := store.Delete(999); return err },
	} {
		if err := operation(); !errors.Is(err, ErrCredentialNotFound) {
			t.Fatalf("operation error = %v, want ErrCredentialNotFound", err)
		}
	}
}

func loadStoreForTest(t *testing.T, path string) *Store {
	t.Helper()
	store, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore() error = %v", err)
	}
	store.now = func() time.Time { return storeTestNow }
	return store
}

func issueStoreRecord(t *testing.T, store *Store, machineID string) Record {
	t.Helper()
	_, record, err := store.Issue("user", machineID, []string{"127.0.0.1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func issueRecordForStoreTest(t *testing.T, id uint64, machineID string) Record {
	t.Helper()
	_, record, err := Issue(id, "user", machineID, []SourceRule{"127.0.0.1"}, nil, storeTestNow, strings.NewReader(strings.Repeat("r", issueRandomBytes)))
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func writeStoreFile(t *testing.T, path string, records []Record) {
	t.Helper()
	encoded, err := json.Marshal(storeFile{Version: storeVersion, Credentials: records})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func formatStoreTestIndex(index int) string {
	const digits = "0123456789"
	if index < 10 {
		return string(digits[index])
	}
	return string([]byte{digits[index/10], digits[index%10]})
}
