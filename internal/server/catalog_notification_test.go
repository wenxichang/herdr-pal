package server

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/hprp"
)

func TestNotificationInventoryIncludesSyncedEmptyMachines(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "a", ClientKey{UserID: "a", MachineID: "mac"}, hprp.SessionSnapshot{Sequence: 1})
	attachSnapshot(t, catalog, "b", ClientKey{UserID: "a", MachineID: "pc"}, hprp.SessionSnapshot{Sequence: 1, Sessions: []hprp.Session{hprpSession(1, "p", "s", "task")}})
	_, _ = catalog.Attach("c", ClientKey{UserID: "a", MachineID: "pending"})
	attachSnapshot(t, catalog, "d", ClientKey{UserID: "b", MachineID: "other"}, hprp.SessionSnapshot{Sequence: 1})
	inventory := catalog.NotificationInventory("a")
	if len(inventory.MachineIDs) != 2 || len(inventory.Entries) != 1 {
		t.Fatalf("inventory=%#v", inventory)
	}
}

func TestNotificationListRequiresDeliveryCommit(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "a", ClientKey{UserID: "a", MachineID: "mac"}, hprp.SessionSnapshot{Sequence: 1, Sessions: []hprp.Session{hprpSession(1, "p", "s", "task")}})
	entries := catalog.CreateNumberedSnapshot("a")
	if _, err := catalog.ResolveNotificationGroups("a", []int{1}); !errors.Is(err, ErrNoListSnapshot) {
		t.Fatalf("err=%v", err)
	}
	if err := catalog.CommitNotificationList("a", entries); err != nil {
		t.Fatal(err)
	}
	result, err := catalog.ResolveNotificationGroups("a", []int{1})
	if err != nil || len(result.Identities) != 1 {
		t.Fatalf("result=%#v %v", result, err)
	}
	catalog.InvalidateNotificationList("a")
	if _, err := catalog.ResolveNotificationGroups("a", []int{1}); !errors.Is(err, ErrNoListSnapshot) {
		t.Fatal(err)
	}
}

func TestNotificationBatchUsesDisplayedIdentityAfterTargetReplacement(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "a", ClientKey{UserID: "a", MachineID: "mac"}, hprp.SessionSnapshot{Sequence: 1, Sessions: []hprp.Session{hprpSession(1, "p1", "s1", "task")}})
	_ = catalog.CommitNotificationList("a", catalog.CreateNumberedSnapshot("a"))
	_ = catalog.ApplySnapshot("a", hprp.SessionSnapshot{Sequence: 2, Sessions: []hprp.Session{hprpSession(1, "p2", "s2", "task"), hprpSession(2, "p3", "s3", "task")}})
	result, err := catalog.ResolveNotificationGroups("a", []int{1, 1, 99})
	if err != nil || len(result.Identities) != 1 || result.AffectedSessions != 2 || len(result.Skipped) != 1 {
		t.Fatalf("result=%#v %v", result, err)
	}
}

func TestNotificationBatchSkipsMissingGroups(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "a", ClientKey{UserID: "a", MachineID: "mac"}, hprp.SessionSnapshot{Sequence: 1, Sessions: []hprp.Session{hprpSession(1, "p1", "s1", "a"), hprpSession(2, "p2", "s2", "b")}})
	attachSnapshot(t, catalog, "b", ClientKey{UserID: "a", MachineID: "pc"}, hprp.SessionSnapshot{Sequence: 1, Sessions: []hprp.Session{hprpSession(1, "p3", "s3", "c")}})
	_ = catalog.CommitNotificationList("a", catalog.CreateNumberedSnapshot("a"))
	catalog.Detach("b")
	_ = catalog.ApplySnapshot("a", hprp.SessionSnapshot{Sequence: 2, Sessions: []hprp.Session{hprpSession(1, "p1", "s1", "a"), hprpSession(2, "p2", "s2", "renamed")}})
	result, err := catalog.ResolveNotificationGroups("a", []int{1, 2, 3, 99})
	if err != nil || len(result.Identities) != 1 || len(result.Skipped) != 3 || result.Identities[0].Title != "a" {
		t.Fatalf("result=%#v %v", result, err)
	}
}

func TestNotificationBatchIgnoresAutomaticRenumbering(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "a", ClientKey{UserID: "a", MachineID: "mac"}, hprp.SessionSnapshot{Sequence: 1, Sessions: []hprp.Session{hprpSession(1, "p1", "s1", "old")}})
	_ = catalog.CommitNotificationList("a", catalog.CreateNumberedSnapshot("a"))
	_ = catalog.ApplySnapshot("a", hprp.SessionSnapshot{Sequence: 2, Sessions: []hprp.Session{hprpSession(1, "p2", "s2", "new"), hprpSession(2, "p1", "s1", "old")}})
	_, _ = catalog.EnsureNumberedIndex("a", hprp.Target{MachineID: "mac", SlotID: "p2", SessionID: "s2"})
	result, err := catalog.ResolveNotificationGroups("a", []int{1})
	if err != nil || len(result.Identities) != 1 || result.Identities[0].Title != "old" {
		t.Fatalf("result=%#v %v", result, err)
	}
}

func TestNotificationListUsesCapturedInventory(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "a", ClientKey{UserID: "a", MachineID: "mac"}, hprp.SessionSnapshot{Sequence: 1, Sessions: []hprp.Session{hprpSession(1, "p", "s", "old")}})
	inventory := catalog.NotificationInventory("a")
	_ = catalog.ApplySnapshot("a", hprp.SessionSnapshot{Sequence: 2, Sessions: []hprp.Session{hprpSession(1, "p", "s", "new")}})
	entries := catalog.createNumberedSnapshotFromEntries("a", inventory.Entries)
	if entries[0].Session.Display.Title != "old" {
		t.Fatal("recaptured mutable directory")
	}
	_ = catalog.CommitNotificationList("a", entries)
	result, err := catalog.ResolveNotificationGroups("a", []int{1})
	if err != nil || len(result.Identities) != 0 || len(result.Skipped) != 1 {
		t.Fatalf("result=%#v %v", result, err)
	}
}

func TestNotificationRetiredSourceIsolationAndBounds(t *testing.T) {
	catalog := NewSessionCatalog()
	now := time.Now()
	catalog.notificationNow = func() time.Time { return now }
	old := hprp.Target{MachineID: "mac", SlotID: "p", SessionID: "s"}
	attachSnapshot(t, catalog, "a", ClientKey{UserID: "a", MachineID: "mac"}, hprp.SessionSnapshot{Sequence: 1, Sessions: []hprp.Session{hprpSession(1, "p", "s", "old")}})
	_ = catalog.ApplySnapshot("a", hprp.SessionSnapshot{Sequence: 2})
	source, err := catalog.ResolveNotificationSource("a", old, true)
	if err != nil || !source.Retired || source.Entry.Session.Display.Title != "old" {
		t.Fatalf("source=%#v %v", source, err)
	}
	if _, err := catalog.ResolveTarget("a", old); err == nil {
		t.Fatal("retired target became actionable")
	}
	if _, err := catalog.ResolveNotificationSource("b", old, true); err == nil {
		t.Fatal("cross user source")
	}
	now = now.Add(10 * time.Minute)
	if _, err := catalog.ResolveNotificationSource("a", old, true); err == nil {
		t.Fatal("retired source did not expire")
	}
	_ = catalog.ApplySnapshot("a", hprp.SessionSnapshot{Sequence: 3, Sessions: []hprp.Session{hprpSession(1, "p", "s", "old")}})
	_ = catalog.ApplySnapshot("a", hprp.SessionSnapshot{Sequence: 4})
	catalog.Detach("a")
	if _, err := catalog.ResolveNotificationSource("a", old, true); err == nil {
		t.Fatal("detach retained source")
	}
}

func TestNotificationRetiredSourceCapacityEvictsOldest(t *testing.T) {
	catalog := NewSessionCatalog()
	now := time.Now()
	catalog.notificationNow = func() time.Time { return now }
	_, _ = catalog.Attach("a", ClientKey{UserID: "a", MachineID: "mac"})
	for seq := 1; seq <= 20; seq++ {
		sessions := make([]hprp.Session, 256)
		for i := range sessions {
			sessions[i] = hprpSession(i+1, fmt.Sprint("pane-", seq, "-", i), fmt.Sprint("occ-", seq, "-", i), "task")
		}
		if err := catalog.ApplySnapshot("a", hprp.SessionSnapshot{Sequence: uint64(seq), Sessions: sessions}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	if len(catalog.retiredNotifications) != 4096 {
		t.Fatalf("retired count=%d", len(catalog.retiredNotifications))
	}
	old := hprp.Target{MachineID: "mac", SlotID: "pane-1-0", SessionID: "occ-1-0"}
	recent := hprp.Target{MachineID: "mac", SlotID: "pane-19-0", SessionID: "occ-19-0"}
	if _, err := catalog.ResolveNotificationSource("a", old, true); err == nil {
		t.Fatal("oldest source retained")
	}
	if _, err := catalog.ResolveNotificationSource("a", recent, true); err != nil {
		t.Fatal("recent source evicted")
	}
}
