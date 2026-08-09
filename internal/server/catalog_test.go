package server

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/hprp"
)

func TestCatalogRejectsDuplicateCompositeKeyButAllowsSameMachineForOtherUser(t *testing.T) {
	catalog := NewSessionCatalog()
	if attached, err := catalog.Attach("conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}); err != nil || !attached {
		t.Fatalf("Attach(first) = %v, %v", attached, err)
	}
	if attached, err := catalog.Attach("conn-2", ClientKey{UserID: "user-a", MachineID: "home-mac"}); attached || !errors.Is(err, ErrDuplicateClient) {
		t.Fatalf("Attach(duplicate) = %v, %v", attached, err)
	}
	if attached, err := catalog.Attach("conn-3", ClientKey{UserID: "user-b", MachineID: "home-mac"}); err != nil || !attached {
		t.Fatalf("Attach(other user) = %v, %v", attached, err)
	}
}

func TestCatalogAppliesOnlyIncreasingFullSnapshots(t *testing.T) {
	catalog := NewSessionCatalog()
	_, _ = catalog.Attach("conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"})
	first := hprp.SessionSnapshot{Sequence: 1, Sessions: []hprp.Session{hprpSession(1, "pane-1", "occ-1", "first")}}
	if err := catalog.ApplySnapshot("conn-1", first); err != nil {
		t.Fatalf("ApplySnapshot(first) error = %v", err)
	}
	if err := catalog.ApplySnapshot("conn-1", hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{hprpSession(1, "ignored-pane", "ignored-session", "ignored")},
	}); err != nil {
		t.Fatalf("ApplySnapshot(idempotent) error = %v", err)
	}
	second := hprp.SessionSnapshot{Sequence: 2, Sessions: []hprp.Session{hprpSession(1, "pane-2", "occ-2", "second")}}
	if err := catalog.ApplySnapshot("conn-1", second); err != nil {
		t.Fatalf("ApplySnapshot(second) error = %v", err)
	}
	if err := catalog.ApplySnapshot("conn-1", first); !errors.Is(err, ErrSnapshotStale) {
		t.Fatalf("ApplySnapshot(stale) error = %v", err)
	}
	entries := catalog.CreateNumberedSnapshot("user-a")
	if len(entries) != 1 || entries[0].Session.SlotID != "pane-2" || entries[0].Session.Display.Title != "second" {
		t.Fatalf("entries = %#v", entries)
	}
}

func TestCatalogStoresOutputModePerFullTarget(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{
			hprpSession(1, "pane-1", "occ-1", "first"),
			hprpSession(2, "pane-2", "occ-2", "second"),
		},
	})
	entries := catalog.CreateNumberedSnapshot("user-a")
	if err := catalog.SetOutputMode("user-a", entries[0].Ref, hprp.OutputModeImage); err != nil {
		t.Fatalf("SetOutputMode() error = %v", err)
	}
	mode, explicit, err := catalog.OutputMode("user-a", entries[0].Ref)
	if err != nil || !explicit || mode != hprp.OutputModeImage {
		t.Fatalf("OutputMode(first) = %q, %v, %v", mode, explicit, err)
	}
	mode, explicit, err = catalog.OutputMode("user-a", entries[1].Ref)
	if err != nil || explicit || mode != "" {
		t.Fatalf("OutputMode(second) = %q, %v, %v", mode, explicit, err)
	}
}

func TestCatalogMigratesExplicitModeAcrossSameSlotReplacement(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{hprpSession(1, "pane-1", "occ-old", "old")},
	})
	oldTarget := catalog.CreateNumberedSnapshot("user-a")[0].Ref
	if err := catalog.SetOutputMode("user-a", oldTarget, hprp.OutputModeImage); err != nil {
		t.Fatal(err)
	}
	if err := catalog.ApplySnapshot("conn-1", hprp.SessionSnapshot{
		Sequence: 2, Sessions: []hprp.Session{hprpSession(1, "pane-1", "occ-new", "new")},
	}); err != nil {
		t.Fatal(err)
	}
	newTarget := catalog.CreateNumberedSnapshot("user-a")[0].Ref
	mode, explicit, err := catalog.OutputMode("user-a", newTarget)
	if err != nil || !explicit || mode != hprp.OutputModeImage {
		t.Fatalf("OutputMode(replacement) = %q, %v, %v", mode, explicit, err)
	}
	if _, _, err := catalog.OutputMode("user-a", oldTarget); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("OutputMode(old) error = %v", err)
	}
}

func TestCatalogDropsModesOnSessionRemovalAndDetach(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{
			hprpSession(1, "pane-1", "occ-1", "first"),
			hprpSession(2, "pane-2", "occ-2", "second"),
		},
	})
	entries := catalog.CreateNumberedSnapshot("user-a")
	for _, entry := range entries {
		if err := catalog.SetOutputMode("user-a", entry.Ref, hprp.OutputModeImage); err != nil {
			t.Fatal(err)
		}
	}
	if err := catalog.ApplySnapshot("conn-1", hprp.SessionSnapshot{
		Sequence: 2, Sessions: []hprp.Session{hprpSession(1, "pane-2", "occ-2", "second")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := catalog.OutputMode("user-a", entries[0].Ref); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("removed mode error = %v", err)
	}
	if mode, explicit, err := catalog.OutputMode("user-a", entries[1].Ref); err != nil || !explicit || mode != hprp.OutputModeImage {
		t.Fatalf("remaining mode = %q, %v, %v", mode, explicit, err)
	}
	if !catalog.Detach("conn-1") {
		t.Fatal("Detach() = false")
	}
	if _, _, err := catalog.OutputMode("user-a", entries[1].Ref); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("detached mode error = %v", err)
	}
}

func TestCatalogCreatesStableCrossMachineNumbering(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "conn-z", ClientKey{UserID: "user-a", MachineID: "z-machine"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{hprpSession(1, "pane-z", "occ-z", "Z")},
	})
	attachSnapshot(t, catalog, "conn-a", ClientKey{UserID: "user-a", MachineID: "a-machine"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{
			hprpSession(2, "pane-a2", "occ-a2", "A2"),
			hprpSession(1, "pane-a1", "occ-a1", "A1"),
		},
	})

	entries := catalog.CreateNumberedSnapshot("user-a")
	got := make([]hprp.Target, len(entries))
	for index := range entries {
		got[index] = entries[index].Ref
	}
	want := []hprp.Target{
		{MachineID: "a-machine", SlotID: "pane-a1", SessionID: "occ-a1"},
		{MachineID: "a-machine", SlotID: "pane-a2", SessionID: "occ-a2"},
		{MachineID: "z-machine", SlotID: "pane-z", SessionID: "occ-z"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("numbering = %#v, want %#v", got, want)
	}
	resolved, err := catalog.ResolveNumbered("user-a", 2)
	if err != nil || resolved.Ref != want[1] {
		t.Fatalf("ResolveNumbered() = %#v, %v", resolved, err)
	}
}

func TestCatalogSelectedAutomaticallyUsesOnlySession(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{hprpSession(1, "pane-1", "occ-1", "only")},
	})

	selected, err := catalog.Selected("user-a")
	if err != nil || selected.Ref != (hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "occ-1"}) {
		t.Fatalf("Selected() = %#v, %v", selected, err)
	}

	attachSnapshot(t, catalog, "conn-2", ClientKey{UserID: "user-a", MachineID: "office-pc"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{hprpSession(1, "pane-2", "occ-2", "second")},
	})
	selected, err = catalog.Selected("user-a")
	if err != nil || selected.Ref.MachineID != "home-mac" {
		t.Fatalf("persisted Selected() = %#v, %v", selected, err)
	}
}

func TestCatalogSelectedStillRequiresExplicitChoiceForMultipleSessions(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{
			hprpSession(1, "pane-1", "occ-1", "first"),
			hprpSession(2, "pane-2", "occ-2", "second"),
		},
	})

	if _, err := catalog.Selected("user-a"); !errors.Is(err, ErrNoSelection) {
		t.Fatalf("Selected() error = %v", err)
	}
}

func TestCatalogDetachInvalidatesNumberingAndSelection(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{hprpSession(1, "pane-1", "occ-1", "title")},
	})
	entries := catalog.CreateNumberedSnapshot("user-a")
	if err := catalog.SetSelection("user-a", entries[0].Ref); err != nil {
		t.Fatalf("SetSelection() error = %v", err)
	}
	if detached := catalog.Detach("conn-1"); !detached {
		t.Fatal("Detach() = false")
	}
	if _, err := catalog.Selected("user-a"); !errors.Is(err, ErrNoSelection) {
		t.Fatalf("Selected() error = %v", err)
	}
	if _, err := catalog.ResolveNumbered("user-a", 1); !errors.Is(err, ErrNoListSnapshot) {
		t.Fatalf("ResolveNumbered() error = %v", err)
	}
	if catalog.HasMachine("user-a", "home-mac") {
		t.Fatal("detached machine remains online")
	}
}

func TestCatalogSelectedAutomaticallyUsesOnlyReplacementAfterOccupantChanges(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{hprpSession(1, "pane-1", "occ-1", "title")},
	})
	entry := catalog.CreateNumberedSnapshot("user-a")[0]
	if err := catalog.SetSelection("user-a", entry.Ref); err != nil {
		t.Fatal(err)
	}
	if err := catalog.ApplySnapshot("conn-1", hprp.SessionSnapshot{
		Sequence: 2, Sessions: []hprp.Session{hprpSession(1, "pane-1", "occ-2", "new")},
	}); err != nil {
		t.Fatal(err)
	}
	selected, err := catalog.Selected("user-a")
	if err != nil || selected.Ref != (hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "occ-2"}) {
		t.Fatalf("Selected() = %#v, %v", selected, err)
	}
	if _, err := catalog.ResolveNumbered("user-a", 1); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("ResolveNumbered(stale occupant) error = %v", err)
	}
}

func TestCatalogRebindSelectionWaitsForReplacementSnapshot(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{hprpSession(1, "pane-1", "occ-1", "old")},
	})
	oldTarget := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "occ-1"}
	newTarget := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "occ-2"}
	if err := catalog.SetSelection("user-a", oldTarget); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- catalog.RebindSelection(ctx, "user-a", oldTarget, newTarget) }()
	select {
	case err := <-done:
		t.Fatalf("RebindSelection() returned before snapshot: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := catalog.ApplySnapshot("conn-1", hprp.SessionSnapshot{
		Sequence: 2, Sessions: []hprp.Session{hprpSession(1, "pane-1", "occ-2", "new")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("RebindSelection() error = %v", err)
	}
	selected, err := catalog.Selected("user-a")
	if err != nil || selected.Ref != newTarget {
		t.Fatalf("Selected() = %#v, %v", selected, err)
	}
}

func TestCatalogRebindSelectionDoesNotOverwriteNewSelection(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{
			hprpSession(1, "pane-1", "occ-1", "old"),
			hprpSession(2, "pane-2", "occ-other", "other"),
		},
	})
	oldTarget := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "occ-1"}
	newTarget := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "occ-2"}
	otherTarget := hprp.Target{MachineID: "home-mac", SlotID: "pane-2", SessionID: "occ-other"}
	if err := catalog.SetSelection("user-a", oldTarget); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- catalog.RebindSelection(ctx, "user-a", oldTarget, newTarget) }()
	time.Sleep(20 * time.Millisecond)
	if err := catalog.SetSelection("user-a", otherTarget); err != nil {
		t.Fatal(err)
	}
	if err := catalog.ApplySnapshot("conn-1", hprp.SessionSnapshot{
		Sequence: 2, Sessions: []hprp.Session{
			hprpSession(1, "pane-1", "occ-2", "new"),
			hprpSession(2, "pane-2", "occ-other", "other"),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("RebindSelection() error = %v", err)
	}
	selected, err := catalog.Selected("user-a")
	if err != nil || selected.Ref != otherTarget {
		t.Fatalf("Selected() = %#v, %v", selected, err)
	}
}

func TestCatalogWaitForTargetDoesNotChangeSelection(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{
			hprpSession(1, "pane-1", "occ-current", "current"),
			hprpSession(2, "pane-2", "occ-old", "old"),
		},
	})
	current := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "occ-current"}
	replacement := hprp.Target{MachineID: "home-mac", SlotID: "pane-2", SessionID: "occ-new"}
	if err := catalog.SetSelection("user-a", current); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := catalog.WaitForTarget(ctx, "user-a", replacement)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("WaitForTarget() returned before snapshot: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := catalog.ApplySnapshot("conn-1", hprp.SessionSnapshot{
		Sequence: 2, Sessions: []hprp.Session{
			hprpSession(1, "pane-1", "occ-current", "current"),
			hprpSession(2, "pane-2", "occ-new", "new"),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	selected, err := catalog.Selected("user-a")
	if err != nil || selected.Ref != current {
		t.Fatalf("Selected() = %#v, %v, want current", selected, err)
	}
}

func TestCatalogSetSelectionWhenAvailableWaitsForSnapshot(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{hprpSession(1, "pane-1", "occ-old", "old")},
	})
	replacement := hprp.Target{MachineID: "home-mac", SlotID: "pane-1", SessionID: "occ-new"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- catalog.SetSelectionWhenAvailable(ctx, "user-a", replacement) }()
	select {
	case err := <-done:
		t.Fatalf("SetSelectionWhenAvailable() returned before snapshot: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := catalog.ApplySnapshot("conn-1", hprp.SessionSnapshot{
		Sequence: 2, Sessions: []hprp.Session{hprpSession(1, "pane-1", "occ-new", "new")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	selected, err := catalog.Selected("user-a")
	if err != nil || selected.Ref != replacement {
		t.Fatalf("Selected() = %#v, %v, want replacement", selected, err)
	}
}

func attachSnapshot(t *testing.T, catalog *SessionCatalog, connectionID string, key ClientKey, snapshot hprp.SessionSnapshot) {
	t.Helper()
	if _, err := catalog.Attach(connectionID, key); err != nil {
		t.Fatal(err)
	}
	if err := catalog.ApplySnapshot(connectionID, snapshot); err != nil {
		t.Fatal(err)
	}
}

func hprpSession(index int, paneID, occupant, title string) hprp.Session {
	return hprp.Session{
		SlotID: paneID, SessionID: occupant,
		Display: hprp.SessionDisplay{Index: index, Agent: "codex", DisplayAgent: "Codex", Title: title, Workspace: "workspace", Tab: "main"},
		Status:  "working",
	}
}

func relaySession(index int, paneID, occupant, title string) hprp.Session {
	return hprpSession(index, paneID, occupant, title)
}
