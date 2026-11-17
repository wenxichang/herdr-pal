package notificationprefs

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/hprp"
)

func identity(t *testing.T, user, machine, title string) Identity {
	t.Helper()
	key, err := NewIdentity(user, machine, hprp.SessionDisplay{Title: title, Workspace: "project", Tab: "web", Agent: "dsh"})
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestIdentityScopesAndNormalizesBusinessFields(t *testing.T) {
	first := identity(t, "user-a", "mac", "task")
	got, err := NewIdentity("user-a", "mac", hprp.SessionDisplay{Title: " task ", Workspace: " project ", Tab: " web ", Agent: " DSH "})
	if err != nil || got != first {
		t.Fatalf("identity=%#v err=%v", got, err)
	}
	for _, key := range []Identity{
		identity(t, "user-b", "mac", "task"), identity(t, "user-a", "pc", "task"), identity(t, "user-a", "mac", "other"),
		{PrincipalID: first.PrincipalID, MachineID: first.MachineID, Title: first.Title, Workspace: "other", Tab: first.Tab, CLIType: first.CLIType},
		{PrincipalID: first.PrincipalID, MachineID: first.MachineID, Title: first.Title, Workspace: first.Workspace, Tab: "desktop", CLIType: first.CLIType},
		{PrincipalID: first.PrincipalID, MachineID: first.MachineID, Title: first.Title, Workspace: first.Workspace, Tab: first.Tab, CLIType: "codex"},
	} {
		if key == first {
			t.Fatal("identity collision")
		}
	}
	fallback, err := NewIdentity("user-a", "mac", hprp.SessionDisplay{DisplayAgent: " Codex "})
	if err != nil || fallback.CLIType != "codex" {
		t.Fatalf("fallback=%#v %v", fallback, err)
	}
	blank, err := NewIdentity("user-a", "mac", hprp.SessionDisplay{})
	if err != nil || blank.CLIType != "unknown" {
		t.Fatalf("blank=%#v %v", blank, err)
	}
	if _, err := NewIdentity("", "mac", hprp.SessionDisplay{}); err == nil {
		t.Fatal("accepted missing user")
	}
}

func TestStoreDefaultsAndBatchRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preferences.json")
	store, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a, b := identity(t, "user-a", "mac", "a"), identity(t, "user-a", "mac", "b")
	if !store.Enabled(a) {
		t.Fatal("default must be on")
	}
	if _, exists := store.Lookup(a); exists {
		t.Fatal("unexpected record")
	}
	if err := store.SetBatch([]Identity{a, b, a}, false); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBatch([]Identity{a}, true); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if on, exists := reloaded.Lookup(a); !on || !exists {
		t.Fatalf("enabled record=%v,%v", on, exists)
	}
	if reloaded.Enabled(b) {
		t.Fatal("disabled state lost")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatal("insecure mode")
	}
}

func TestStoreBatchFailureIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preferences.json")
	store, _ := LoadStore(path)
	a, b := identity(t, "user-a", "mac", "a"), identity(t, "user-a", "mac", "b")
	if err := store.SetBatch([]Identity{a}, false); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	store.replaceFile = func(string, string) error { return errors.New("disk failure") }
	if err := store.SetBatch([]Identity{a, b}, true); err == nil {
		t.Fatal("expected failure")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) || store.Enabled(a) {
		t.Fatal("failed transaction changed state")
	}
	if _, exists := store.Lookup(b); exists {
		t.Fatal("partial insert")
	}
}

func TestStoreReconcileAddsPreservesAndPrunesScopedGroups(t *testing.T) {
	store := NewMemoryStore()
	old, keep, newKey := identity(t, "a", "mac", "old"), identity(t, "a", "mac", "keep"), identity(t, "a", "mac", "new")
	offline, other := identity(t, "a", "pc", "offline"), identity(t, "b", "mac", "other")
	if err := store.SetBatch([]Identity{old, keep, offline, other}, false); err != nil {
		t.Fatal(err)
	}
	result, err := store.Reconcile("a", []string{"mac"}, []Identity{keep, newKey, newKey})
	if err != nil || result.Added != 1 || result.Removed != 1 || result.States[keep] || !result.States[newKey] {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, exists := store.Lookup(old); exists {
		t.Fatal("stale group retained")
	}
	for _, key := range []Identity{offline, other} {
		if on, exists := store.Lookup(key); on || !exists {
			t.Fatal("unrelated record changed")
		}
	}
	result.States[keep] = true
	if store.Enabled(keep) {
		t.Fatal("mutable result escaped")
	}
}

func TestStoreReconcilePreservesEmptyAndOfflineMachines(t *testing.T) {
	store := NewMemoryStore()
	key := identity(t, "a", "mac", "task")
	_ = store.SetBatch([]Identity{key}, false)
	if _, err := store.Reconcile("a", nil, nil); err != nil {
		t.Fatal(err)
	}
	if store.Enabled(key) {
		t.Fatal("offline preference lost")
	}
	result, err := store.Reconcile("a", []string{"mac"}, nil)
	if err != nil || result.Removed != 0 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if enabled, exists := store.Lookup(key); enabled || !exists {
		t.Fatal("empty machine lost persisted mute")
	}
}

func TestStoreReconcileFailureIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.json")
	store, _ := LoadStore(path)
	old, next := identity(t, "a", "mac", "old"), identity(t, "a", "mac", "new")
	_ = store.SetBatch([]Identity{old}, false)
	store.replaceFile = func(string, string) error { return errors.New("replace failure") }
	if _, err := store.Reconcile("a", []string{"mac"}, []Identity{next}); err == nil {
		t.Fatal("expected failure")
	}
	if on, exists := store.Lookup(old); on || !exists {
		t.Fatal("removed on failure")
	}
	if _, exists := store.Lookup(next); exists {
		t.Fatal("inserted on failure")
	}
}

func TestStoreConcurrentUpdatesPreserveAllGroups(t *testing.T) {
	store := NewMemoryStore()
	var wg sync.WaitGroup
	for _, title := range []string{"a", "b", "c", "d"} {
		key := identity(t, "a", "mac", title)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.SetBatch([]Identity{key}, false); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for _, title := range []string{"a", "b", "c", "d"} {
		if store.Enabled(identity(t, "a", "mac", title)) {
			t.Fatal("lost update")
		}
	}
}

func TestStoreRejectsInvalidFile(t *testing.T) {
	for _, body := range []string{"secret invalid", `{"version":9,"records":[]}`, `{"version":1,"records":[]} {}`, `{"version":1,"records":[{}]}`} {
		path := filepath.Join(t.TempDir(), "prefs.json")
		_ = os.WriteFile(path, []byte(body), 0o600)
		if _, err := LoadStore(path); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	path := filepath.Join(t.TempDir(), "prefs.json")
	_ = os.WriteFile(path, []byte(`{"version":1,"records":[]}`), 0o644)
	if _, err := LoadStore(path); err == nil {
		t.Fatal("accepted insecure permissions")
	}
}

func TestStoreSyncFailureDoesNotPublish(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.json")
	store, _ := LoadStore(path)
	key := identity(t, "a", "mac", "task")
	_ = store.SetBatch([]Identity{key}, false)
	before, _ := os.ReadFile(path)
	store.syncFile = func(*os.File) error { return errors.New("sync failure") }
	if err := store.SetBatch([]Identity{key}, true); err == nil {
		t.Fatal("expected sync failure")
	}
	after, _ := os.ReadFile(path)
	if store.Enabled(key) || string(before) != string(after) {
		t.Fatal("failed sync published transaction")
	}
}

func TestStoreReconcileRequiresNonemptyListForEachMachine(t *testing.T) {
	store := NewMemoryStore()
	empty := identity(t, "a", "empty", "muted")
	stale := identity(t, "a", "active", "stale")
	current := identity(t, "a", "active", "current")
	offline := identity(t, "a", "offline", "muted")
	other := identity(t, "b", "active", "muted")
	if err := store.SetBatch([]Identity{empty, stale, offline, other}, false); err != nil {
		t.Fatal(err)
	}
	result, err := store.Reconcile("a", []string{"empty", "active"}, []Identity{current})
	if err != nil || result.Added != 1 || result.Removed != 1 || len(result.RemovedIdentities) != 1 || result.RemovedIdentities[0] != stale {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	for _, key := range []Identity{empty, offline, other} {
		if enabled, exists := store.Lookup(key); enabled || !exists {
			t.Fatalf("preserved key lost: %#v", key)
		}
	}
	if _, exists := store.Lookup(stale); exists {
		t.Fatal("nonempty machine did not prune stale group")
	}
	if !result.States[current] {
		t.Fatal("new group not enabled")
	}
}
