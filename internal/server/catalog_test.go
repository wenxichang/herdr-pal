package server

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/relayproto"
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
	first := relayproto.SessionSnapshot{Sequence: 1, Sessions: []relayproto.Session{relaySession(1, "pane-1", "occ-1", "first")}}
	if err := catalog.ApplySnapshot("conn-1", first); err != nil {
		t.Fatalf("ApplySnapshot(first) error = %v", err)
	}
	if err := catalog.ApplySnapshot("conn-1", first); !errors.Is(err, ErrSnapshotStale) {
		t.Fatalf("ApplySnapshot(stale) error = %v", err)
	}
	second := relayproto.SessionSnapshot{Sequence: 2, Sessions: []relayproto.Session{relaySession(1, "pane-2", "occ-2", "second")}}
	if err := catalog.ApplySnapshot("conn-1", second); err != nil {
		t.Fatalf("ApplySnapshot(second) error = %v", err)
	}
	entries := catalog.CreateNumberedSnapshot("user-a")
	if len(entries) != 1 || entries[0].Session.PaneID != "pane-2" || entries[0].Session.Title != "second" {
		t.Fatalf("entries = %#v", entries)
	}
}

func TestCatalogCreatesStableCrossMachineNumbering(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "conn-z", ClientKey{UserID: "user-a", MachineID: "z-machine"}, relayproto.SessionSnapshot{
		Sequence: 1, Sessions: []relayproto.Session{relaySession(1, "pane-z", "occ-z", "Z")},
	})
	attachSnapshot(t, catalog, "conn-a", ClientKey{UserID: "user-a", MachineID: "a-machine"}, relayproto.SessionSnapshot{
		Sequence: 1, Sessions: []relayproto.Session{
			relaySession(2, "pane-a2", "occ-a2", "A2"),
			relaySession(1, "pane-a1", "occ-a1", "A1"),
		},
	})

	entries := catalog.CreateNumberedSnapshot("user-a")
	got := make([]relayproto.SessionRef, len(entries))
	for index := range entries {
		got[index] = entries[index].Ref
	}
	want := []relayproto.SessionRef{
		{MachineID: "a-machine", LocalIndex: 1, PaneID: "pane-a1", OccupantHash: "occ-a1"},
		{MachineID: "a-machine", LocalIndex: 2, PaneID: "pane-a2", OccupantHash: "occ-a2"},
		{MachineID: "z-machine", LocalIndex: 1, PaneID: "pane-z", OccupantHash: "occ-z"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("numbering = %#v, want %#v", got, want)
	}
	resolved, err := catalog.ResolveNumbered("user-a", 2)
	if err != nil || resolved.Ref != want[1] {
		t.Fatalf("ResolveNumbered() = %#v, %v", resolved, err)
	}
}

func TestCatalogDetachInvalidatesNumberingAndSelection(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, relayproto.SessionSnapshot{
		Sequence: 1, Sessions: []relayproto.Session{relaySession(1, "pane-1", "occ-1", "title")},
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

func TestCatalogInvalidatesSelectionWhenOccupantChanges(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, relayproto.SessionSnapshot{
		Sequence: 1, Sessions: []relayproto.Session{relaySession(1, "pane-1", "occ-1", "title")},
	})
	entry := catalog.CreateNumberedSnapshot("user-a")[0]
	if err := catalog.SetSelection("user-a", entry.Ref); err != nil {
		t.Fatal(err)
	}
	if err := catalog.ApplySnapshot("conn-1", relayproto.SessionSnapshot{
		Sequence: 2, Sessions: []relayproto.Session{relaySession(1, "pane-1", "occ-2", "new")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Selected("user-a"); !errors.Is(err, ErrNoSelection) {
		t.Fatalf("Selected() error = %v", err)
	}
	if _, err := catalog.ResolveNumbered("user-a", 1); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("ResolveNumbered(stale occupant) error = %v", err)
	}
}

func TestCatalogRebindSelectionWaitsForReplacementSnapshot(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, relayproto.SessionSnapshot{
		Sequence: 1, Sessions: []relayproto.Session{relaySession(1, "pane-1", "occ-1", "old")},
	})
	oldTarget := relayproto.SessionRef{MachineID: "home-mac", LocalIndex: 1, PaneID: "pane-1", OccupantHash: "occ-1"}
	newTarget := relayproto.SessionRef{MachineID: "home-mac", LocalIndex: 1, PaneID: "pane-1", OccupantHash: "occ-2"}
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
	if err := catalog.ApplySnapshot("conn-1", relayproto.SessionSnapshot{
		Sequence: 2, Sessions: []relayproto.Session{relaySession(1, "pane-1", "occ-2", "new")},
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
	attachSnapshot(t, catalog, "conn-1", ClientKey{UserID: "user-a", MachineID: "home-mac"}, relayproto.SessionSnapshot{
		Sequence: 1, Sessions: []relayproto.Session{
			relaySession(1, "pane-1", "occ-1", "old"),
			relaySession(2, "pane-2", "occ-other", "other"),
		},
	})
	oldTarget := relayproto.SessionRef{MachineID: "home-mac", LocalIndex: 1, PaneID: "pane-1", OccupantHash: "occ-1"}
	newTarget := relayproto.SessionRef{MachineID: "home-mac", LocalIndex: 1, PaneID: "pane-1", OccupantHash: "occ-2"}
	otherTarget := relayproto.SessionRef{MachineID: "home-mac", LocalIndex: 2, PaneID: "pane-2", OccupantHash: "occ-other"}
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
	if err := catalog.ApplySnapshot("conn-1", relayproto.SessionSnapshot{
		Sequence: 2, Sessions: []relayproto.Session{
			relaySession(1, "pane-1", "occ-2", "new"),
			relaySession(2, "pane-2", "occ-other", "other"),
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

func attachSnapshot(t *testing.T, catalog *SessionCatalog, connectionID string, key ClientKey, snapshot relayproto.SessionSnapshot) {
	t.Helper()
	if _, err := catalog.Attach(connectionID, key); err != nil {
		t.Fatal(err)
	}
	if err := catalog.ApplySnapshot(connectionID, snapshot); err != nil {
		t.Fatal(err)
	}
}

func relaySession(index int, paneID, occupant, title string) relayproto.Session {
	return relayproto.Session{
		LocalIndex: index, PaneID: paneID, TerminalID: "terminal-" + paneID, OccupantHash: occupant,
		Agent: "codex", DisplayAgent: "Codex", Title: title, Workspace: "workspace", Tab: "main", Status: "working",
	}
}
