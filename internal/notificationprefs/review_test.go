package notificationprefs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreRejectsAmbiguousAndNoncanonicalRecords(t *testing.T) {
	key := identity(t, "user-a", "mac", "task")
	off, on := false, true
	upper := key
	upper.CLIType = "DSH"
	for _, test := range []struct {
		name string
		body any
	}{
		{"duplicate identity", preferenceFile{Version: 1, Records: []preferenceRecord{{Identity: key, Enabled: &off}, {Identity: key, Enabled: &on}}}},
		{"noncanonical identity", preferenceFile{Version: 1, Records: []preferenceRecord{{Identity: upper, Enabled: &off}}}},
		{"missing enabled", map[string]any{"version": 1, "records": []any{map[string]any{"identity": key}}}},
		{"null enabled", map[string]any{"version": 1, "records": []any{map[string]any{"identity": key, "enabled": nil}}}},
		{"unknown field", map[string]any{"version": 1, "records": []any{}, "unexpected": true}},
		{"null record", map[string]any{"version": 1, "records": []any{nil}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "prefs.json")
			data, err := json.Marshal(test.body)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadStore(path); !errors.Is(err, ErrInvalidPreference) {
				t.Fatalf("LoadStore error=%v", err)
			}
		})
	}
}

func TestStoreRejectsSymlinkAndDirectory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	link := filepath.Join(dir, "link.json")
	if err := os.WriteFile(target, []byte(`{"version":1,"records":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, dir} {
		if _, err := LoadStore(path); !errors.Is(err, ErrInvalidPreference) {
			t.Fatalf("accepted nonregular path %s: %v", path, err)
		}
	}
}

func TestStoreRejectsInvalidScopeWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.json")
	store, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	old, newKey := identity(t, "a", "mac", "old"), identity(t, "a", "mac", "new")
	if err := store.SetBatch([]Identity{old}, false); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	invalid := newKey
	invalid.PrincipalID = ""
	foreign := newKey
	foreign.PrincipalID = "b"
	outside := newKey
	outside.MachineID = "pc"
	cases := []struct {
		name string
		run  func() error
	}{
		{"batch with invalid identity", func() error { return store.SetBatch([]Identity{newKey, invalid}, false) }},
		{"invalid principal", func() error { _, err := store.Reconcile("", []string{"mac"}, nil); return err }},
		{"invalid machine", func() error { _, err := store.Reconcile("a", []string{"../bad"}, nil); return err }},
		{"foreign principal", func() error { _, err := store.Reconcile("a", []string{"mac"}, []Identity{foreign}); return err }},
		{"outside machine scope", func() error { _, err := store.Reconcile("a", []string{"mac"}, []Identity{outside}); return err }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); !errors.Is(err, ErrInvalidPreference) {
				t.Fatalf("error=%v", err)
			}
			if enabled, exists := store.Lookup(old); enabled || !exists {
				t.Fatal("invalid input cleared old state")
			}
			if _, exists := store.Lookup(newKey); exists {
				t.Fatal("invalid batch partially inserted")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("invalid input changed disk")
			}
		})
	}
}

func TestStoreNoopUpdatesDoNotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.json")
	store, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	key := identity(t, "a", "mac", "task")
	if err := store.SetBatch([]Identity{key}, false); err != nil {
		t.Fatal(err)
	}
	store.syncFile = func(*os.File) error { return errors.New("unexpected write") }
	store.replaceFile = func(string, string) error { return errors.New("unexpected replace") }
	if err := store.SetBatch([]Identity{key, key}, false); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBatch(nil, true); err != nil {
		t.Fatal(err)
	}
	result, err := store.Reconcile("a", []string{"mac"}, []Identity{key, key})
	if err != nil || result.Added != 0 || result.Removed != 0 || result.States[key] {
		t.Fatalf("result=%#v %v", result, err)
	}
}

func TestStoreEmptyFileCanBeLoaded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"records":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	key := identity(t, "a", "mac", "task")
	if enabled, exists := store.Lookup(key); !enabled || exists {
		t.Fatalf("empty state=%v,%v", enabled, exists)
	}
}
