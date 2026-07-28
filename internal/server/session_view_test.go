package server

import (
	"errors"
	"reflect"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/hprp"
)

func TestManagementSessionsUsesPerPrincipalNumberingAndListOrdering(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "conn-a-z", ClientKey{UserID: "user-a", MachineID: "z-machine"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{managementSession(1, "pane-z", "session-z", hprp.StatusDone)},
	})
	attachSnapshot(t, catalog, "conn-a-a", ClientKey{UserID: "user-a", MachineID: "a-machine"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{
			managementSession(2, "pane-a2", "session-a2", hprp.StatusBlocked),
			managementSession(1, "pane-a1", "session-a1", hprp.StatusWorking),
		},
	})
	attachSnapshot(t, catalog, "conn-b", ClientKey{UserID: "user-b", MachineID: "a-machine"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{managementSession(1, "pane-b", "session-b", "future-status")},
	})

	views := catalog.ManagementSessions(SessionFilter{})
	got := make([]struct {
		principal string
		number    int
		target    hprp.Target
		status    string
		label     string
	}, len(views))
	for index, view := range views {
		got[index] = struct {
			principal string
			number    int
			target    hprp.Target
			status    string
			label     string
		}{view.PrincipalID, view.Number, view.Target, view.Session.Status, view.StatusLabel}
	}
	want := []struct {
		principal string
		number    int
		target    hprp.Target
		status    string
		label     string
	}{
		{"user-a", 1, hprp.Target{MachineID: "a-machine", SlotID: "pane-a1", SessionID: "session-a1"}, hprp.StatusWorking, "working ⏳"},
		{"user-a", 2, hprp.Target{MachineID: "a-machine", SlotID: "pane-a2", SessionID: "session-a2"}, hprp.StatusBlocked, "blocked ⁉️"},
		{"user-a", 3, hprp.Target{MachineID: "z-machine", SlotID: "pane-z", SessionID: "session-z"}, hprp.StatusDone, "done ✅"},
		{"user-b", 1, hprp.Target{MachineID: "a-machine", SlotID: "pane-b", SessionID: "session-b"}, hprp.StatusUnknown, "unknown ❔"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ManagementSessions() = %#v, want %#v", got, want)
	}
	if views[0].WorkspaceLabel != "workspace/main" {
		t.Fatalf("WorkspaceLabel = %q", views[0].WorkspaceLabel)
	}
}

func TestManagementSessionsFiltersWithoutChangingRoutingState(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "conn-a", ClientKey{UserID: "user-a", MachineID: "home"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{managementSession(1, "pane-a", "session-a", hprp.StatusIdle)},
	})
	attachSnapshot(t, catalog, "conn-b", ClientKey{UserID: "user-b", MachineID: "office"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{managementSession(1, "pane-b", "session-b", hprp.StatusDone)},
	})
	selectedTarget := hprp.Target{MachineID: "home", SlotID: "pane-a", SessionID: "session-a"}
	if err := catalog.SetSelection("user-a", selectedTarget); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.ResolveNumbered("user-a", 1); !errors.Is(err, ErrNoListSnapshot) {
		t.Fatalf("ResolveNumbered(before) error = %v", err)
	}

	views := catalog.ManagementSessions(SessionFilter{PrincipalID: "user-b", MachineID: "office"})
	if len(views) != 1 || views[0].PrincipalID != "user-b" || views[0].Target.MachineID != "office" {
		t.Fatalf("filtered views = %#v", views)
	}
	if _, err := catalog.ResolveNumbered("user-a", 1); !errors.Is(err, ErrNoListSnapshot) {
		t.Fatalf("ResolveNumbered(after) error = %v", err)
	}
	selected, err := catalog.Selected("user-a")
	if err != nil || selected.Ref != selectedTarget {
		t.Fatalf("Selected() = %#v, %v", selected, err)
	}
}

func TestManagementSessionsReturnsDeepCopy(t *testing.T) {
	catalog := NewSessionCatalog()
	attachSnapshot(t, catalog, "conn", ClientKey{UserID: "user", MachineID: "home"}, hprp.SessionSnapshot{
		Sequence: 1, Sessions: []hprp.Session{managementSession(1, "pane", "session", hprp.StatusIdle)},
	})
	views := catalog.ManagementSessions(SessionFilter{})
	views[0].Session.Display.Title = "mutated"
	again := catalog.ManagementSessions(SessionFilter{})
	if again[0].Session.Display.Title != "title" {
		t.Fatalf("ManagementSessions() leaked mutable state: %#v", again[0])
	}
}

func managementSession(index int, slotID, sessionID, status string) hprp.Session {
	return hprp.Session{
		SlotID: slotID, SessionID: sessionID, Status: status,
		Display: hprp.SessionDisplay{
			Index: index, Agent: "codex", DisplayAgent: "Codex", Workspace: "workspace", Tab: "main", Title: "title",
		},
	}
}
