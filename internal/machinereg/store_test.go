package machinereg

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/credential"
)

func TestStoreCreatesPersistsAndDeletesPendingRegistration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registrations.json")
	store, err := LoadStore(path, StoreOptions{
		Now:    func() time.Time { return time.Unix(100, 0).UTC() },
		Random: bytes.NewReader(bytes.Repeat([]byte{0xab}, registrationIDRandomBytes)),
	})
	if err != nil {
		t.Fatal(err)
	}
	request, created, err := store.Create("user-a", "office-laptop", []credential.SourceRule{"192.168.0.1"})
	if err != nil || !created || !strings.HasPrefix(request.RegistrationID, "reg_") {
		t.Fatalf("request=%#v created=%t err=%v", request, created, err)
	}
	duplicate, created, err := store.Create("user-a", "office-laptop", []credential.SourceRule{"10.0.0.1"})
	if err != nil || created || duplicate.RegistrationID != request.RegistrationID || duplicate.AllowedSources[0] != "192.168.0.1" {
		t.Fatalf("duplicate=%#v created=%t err=%v", duplicate, created, err)
	}
	reloaded, err := LoadStore(path, StoreOptions{})
	if err != nil || len(reloaded.List()) != 1 || !reloaded.HasPrincipal("user-a") {
		t.Fatalf("list=%#v err=%v", reloaded.List(), err)
	}
	shown, err := reloaded.Show(request.RegistrationID)
	if err != nil || shown.RegistrationID != request.RegistrationID {
		t.Fatalf("Show()=%#v err=%v", shown, err)
	}
	found, ok := reloaded.Find("user-a", "office-laptop")
	if !ok || found.RegistrationID != request.RegistrationID {
		t.Fatalf("Find()=%#v ok=%t", found, ok)
	}
	deleted, err := reloaded.Delete(request.RegistrationID)
	if err != nil || deleted.RegistrationID != request.RegistrationID || len(reloaded.List()) != 0 {
		t.Fatalf("delete=%#v err=%v list=%#v", deleted, err, reloaded.List())
	}
	if _, err := reloaded.Show(request.RegistrationID); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("Show(deleted) error=%v", err)
	}
}

func TestStoreRejectsInvalidFilesAndPermissions(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "unknown field", body: `{"version":1,"registrations":[],"extra":true}`},
		{name: "wrong version", body: `{"version":2,"registrations":[]}`},
		{name: "trailing data", body: `{"version":1,"registrations":[]} {}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "registrations.json")
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadStore(path, StoreOptions{}); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("LoadStore() error=%v", err)
			}
		})
	}

	if runtime.GOOS != "windows" {
		path := filepath.Join(t.TempDir(), "registrations.json")
		if err := os.WriteFile(path, []byte(`{"version":1,"registrations":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadStore(path, StoreOptions{}); !errors.Is(err, ErrInsecurePermissions) {
			t.Fatalf("LoadStore() error=%v", err)
		}
	}
}

func TestStoreRejectsInvalidRequestsAndExhaustedRandomSource(t *testing.T) {
	store, err := LoadStore(filepath.Join(t.TempDir(), "registrations.json"), StoreOptions{
		Random: bytes.NewReader(bytes.Repeat([]byte{1}, registrationIDRandomBytes)),
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		principal string
		machine   string
		sources   []credential.SourceRule
	}{
		{principal: "", machine: "office", sources: []credential.SourceRule{"127.0.0.1"}},
		{principal: " user", machine: "office", sources: []credential.SourceRule{"127.0.0.1"}},
		{principal: "user", machine: "bad machine", sources: []credential.SourceRule{"127.0.0.1"}},
		{principal: "user", machine: "office"},
		{principal: "user", machine: "office", sources: []credential.SourceRule{"192.168.1.42/24"}},
	}
	for _, test := range tests {
		if _, _, err := store.Create(test.principal, test.machine, test.sources); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Create(%q,%q,%#v) error=%v", test.principal, test.machine, test.sources, err)
		}
	}

	if _, _, err := store.Create("user", "office", []credential.SourceRule{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Create("user", "mobile", []credential.SourceRule{"127.0.0.1"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Create() exhausted random error=%v", err)
	}
}

func TestStoreRetriesRegistrationIDCollisionWithoutOverwritingExistingRequest(t *testing.T) {
	firstID := bytes.Repeat([]byte{1}, registrationIDRandomBytes)
	secondID := bytes.Repeat([]byte{2}, registrationIDRandomBytes)
	random := append(append(append([]byte(nil), firstID...), firstID...), secondID...)
	store, err := LoadStore(filepath.Join(t.TempDir(), "registrations.json"), StoreOptions{
		Random: bytes.NewReader(random),
	})
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := store.Create("user", "office", []credential.SourceRule{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	second, created, err := store.Create("user", "mobile", []credential.SourceRule{"127.0.0.2"})
	if err != nil || !created {
		t.Fatalf("second=%#v created=%t err=%v", second, created, err)
	}
	if second.RegistrationID == first.RegistrationID {
		t.Fatalf("registration ID collision was not retried: %q", second.RegistrationID)
	}
	listed := store.List()
	if len(listed) != 2 || listed[0].MachineID != "office" || listed[1].MachineID != "mobile" {
		t.Fatalf("requests=%#v", listed)
	}
}

func TestStoreRejectsRepeatedRegistrationIDCollisionsWithoutOverwriting(t *testing.T) {
	identifier := bytes.Repeat([]byte{5}, registrationIDRandomBytes)
	random := bytes.NewReader(bytes.Repeat(identifier, registrationIDMaxAttempts+1))
	store, err := LoadStore(filepath.Join(t.TempDir(), "registrations.json"), StoreOptions{Random: random})
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := store.Create("user", "office", []credential.SourceRule{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Create("user", "mobile", []credential.SourceRule{"127.0.0.2"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Create() error=%v", err)
	}
	listed := store.List()
	if len(listed) != 1 || listed[0].RegistrationID != first.RegistrationID || listed[0].MachineID != "office" {
		t.Fatalf("requests=%#v", listed)
	}
}

func TestStoreCreatePersistenceFailureDoesNotChangeMemory(t *testing.T) {
	store, err := LoadStore(filepath.Join(t.TempDir(), "registrations.json"), StoreOptions{
		Random: bytes.NewReader(bytes.Repeat([]byte{3}, registrationIDRandomBytes*2)),
	})
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := store.Create("user", "office", []credential.SourceRule{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	store.path = blockedStorePath(t)
	if _, _, err := store.Create("user", "mobile", []credential.SourceRule{"127.0.0.2"}); err == nil {
		t.Fatal("Create() succeeded with blocked persistence path")
	}
	listed := store.List()
	if len(listed) != 1 || listed[0].RegistrationID != first.RegistrationID {
		t.Fatalf("memory changed after persistence failure: %#v", listed)
	}
}

func TestStoreDeletePersistenceFailureDoesNotChangeMemory(t *testing.T) {
	store, err := LoadStore(filepath.Join(t.TempDir(), "registrations.json"), StoreOptions{
		Random: bytes.NewReader(bytes.Repeat([]byte{4}, registrationIDRandomBytes)),
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _, err := store.Create("user", "office", []credential.SourceRule{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	store.path = blockedStorePath(t)
	if _, err := store.Delete(request.RegistrationID); err == nil {
		t.Fatal("Delete() succeeded with blocked persistence path")
	}
	shown, err := store.Show(request.RegistrationID)
	if err != nil || shown.RegistrationID != request.RegistrationID {
		t.Fatalf("pending changed after persistence failure: %#v err=%v", shown, err)
	}
}

func TestStoreListsRequestsInStableOrderAndReturnsCopies(t *testing.T) {
	times := []time.Time{time.Unix(200, 0).UTC(), time.Unix(100, 0).UTC(), time.Unix(100, 0).UTC()}
	index := 0
	store, err := LoadStore(filepath.Join(t.TempDir(), "registrations.json"), StoreOptions{
		Now: func() time.Time {
			value := times[index]
			index++
			return value
		},
		Random: bytes.NewReader(append(
			append(bytes.Repeat([]byte{3}, registrationIDRandomBytes), bytes.Repeat([]byte{2}, registrationIDRandomBytes)...),
			bytes.Repeat([]byte{1}, registrationIDRandomBytes)...,
		)),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, machine := range []string{"three", "two", "one"} {
		if _, _, err := store.Create("user", machine, []credential.SourceRule{"127.0.0.1"}); err != nil {
			t.Fatal(err)
		}
	}
	list := store.List()
	got := []string{list[0].MachineID, list[1].MachineID, list[2].MachineID}
	if want := []string{"one", "two", "three"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order=%v want=%v", got, want)
	}
	list[0].AllowedSources[0] = "10.0.0.1"
	again := store.List()
	if again[0].AllowedSources[0] != "127.0.0.1" {
		t.Fatalf("List() returned aliased sources: %#v", again[0])
	}
}

func TestStoreConcurrentCreateProducesOnePendingRequest(t *testing.T) {
	store, err := LoadStore(filepath.Join(t.TempDir(), "registrations.json"), StoreOptions{
		Random: bytes.NewReader(bytes.Repeat([]byte{7}, registrationIDRandomBytes)),
	})
	if err != nil {
		t.Fatal(err)
	}
	const workers = 32
	var wait sync.WaitGroup
	wait.Add(workers)
	results := make(chan Request, workers)
	errorsSeen := make(chan error, workers)
	for range workers {
		go func() {
			defer wait.Done()
			request, _, err := store.Create("user", "office", []credential.SourceRule{"127.0.0.1"})
			if err != nil {
				errorsSeen <- err
				return
			}
			results <- request
		}()
	}
	wait.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatalf("Create() error=%v", err)
	}
	var registrationID string
	for request := range results {
		if registrationID == "" {
			registrationID = request.RegistrationID
		}
		if request.RegistrationID != registrationID {
			t.Fatalf("registration IDs differ: %q != %q", request.RegistrationID, registrationID)
		}
	}
	if len(store.List()) != 1 {
		t.Fatalf("List()=%#v", store.List())
	}
}

func blockedStorePath(t *testing.T) string {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(blocker, "registrations.json")
}
