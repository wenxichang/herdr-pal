package server

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/notificationprefs"
)

func TestRouterNotificationFiltersExactBusinessIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*hprp.Session, *string, *string)
		muted  bool
	}{
		{"same identity", func(*hprp.Session, *string, *string) {}, true},
		{"other principal", func(_ *hprp.Session, user, _ *string) { *user = "user-b" }, false},
		{"other machine", func(_ *hprp.Session, _, machine *string) { *machine = "pc" }, false},
		{"other title", func(s *hprp.Session, _, _ *string) { s.Display.Title = "different" }, false},
		{"other workspace", func(s *hprp.Session, _, _ *string) { s.Display.Workspace = "different" }, false},
		{"other tab", func(s *hprp.Session, _, _ *string) { s.Display.Tab = "different" }, false},
		{"other CLI", func(s *hprp.Session, _, _ *string) { s.Display.Agent = "claude" }, false},
		{"new target IDs", func(s *hprp.Session, _, _ *string) {
			s.SlotID = "replacement-pane"
			s.SessionID = "replacement-session"
		}, true},
		{"new display index", func(s *hprp.Session, _, _ *string) { s.Display.Index = 7 }, true},
		{"normalized labels", func(s *hprp.Session, _, _ *string) { s.Display.Title = " task "; s.Display.Agent = " CODEX " }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			router, gateway, relay := newRouterHarness(t)
			current := relaySession(1, "pane", "session", "task")
			key, err := notificationprefs.NewIdentity("user-a", "mac", current.Display)
			if err != nil {
				t.Fatal(err)
			}
			if err := router.notificationPreferences.SetBatch([]notificationprefs.Identity{key}, false); err != nil {
				t.Fatal(err)
			}
			user, machine := "user-a", "mac"
			test.change(&current, &user, &machine)
			attachSnapshot(t, router.catalog, "connection", ClientKey{UserID: user, MachineID: machine}, hprp.SessionSnapshot{Sequence: 1, Sessions: []hprp.Session{current}})
			target := hprp.Target{MachineID: machine, SlotID: current.SlotID, SessionID: current.SessionID}
			if err := router.SendNotification(context.Background(), user, machine, statusNotification(target, "idle", "working")); err != nil {
				t.Fatal(err)
			}
			want := 1
			if test.muted {
				want = 0
			}
			if gateway.ReplyCount() != want || len(relay.FetchCalls()) != 0 {
				t.Fatalf("replies=%v fetches=%v", gateway.Replies(), relay.FetchCalls())
			}
		})
	}
}

func TestRouterListKeepsSharedSwitchUntilNonemptySnapshotConfirmsRemoval(t *testing.T) {
	router, gateway, _ := newRouterHarness(t)
	first, second := relaySession(1, "pane1", "s1", "shared"), relaySession(2, "pane2", "s2", "shared")
	attachSnapshot(t, router.catalog, "connection", ClientKey{UserID: "user-a", MachineID: "mac"}, hprp.SessionSnapshot{Sequence: 1, Sessions: []hprp.Session{first, second}})
	notifyCommand(router, "/ls")
	notifyCommand(router, "/notify off 1")
	key, err := notificationprefs.NewIdentity("user-a", "mac", first.Display)
	if err != nil {
		t.Fatal(err)
	}
	if err := router.catalog.ApplySnapshot("connection", hprp.SessionSnapshot{Sequence: 2, Sessions: []hprp.Session{second}}); err != nil {
		t.Fatal(err)
	}
	notifyCommand(router, "/ls")
	if enabled, exists := router.notificationPreferences.Lookup(key); enabled || !exists {
		t.Fatal("closing one session deleted shared mute")
	}
	before := gateway.ReplyCount()
	old := hprp.Target{MachineID: "mac", SlotID: first.SlotID, SessionID: first.SessionID}
	if err := router.SendNotification(context.Background(), "user-a", "mac", invalidatedNotification(old)); err != nil {
		t.Fatal(err)
	}
	if gateway.ReplyCount() != before {
		t.Fatal("shared muted invalidation leaked")
	}
	if err := router.catalog.ApplySnapshot("connection", hprp.SessionSnapshot{Sequence: 3}); err != nil {
		t.Fatal(err)
	}
	notifyCommand(router, "/ls")
	if enabled, exists := router.notificationPreferences.Lookup(key); enabled || !exists {
		t.Fatal("empty directory discarded shared mute")
	}
	other := relaySession(1, "other-pane", "other-session", "other group")
	if err := router.catalog.ApplySnapshot("connection", hprp.SessionSnapshot{Sequence: 4, Sessions: []hprp.Session{other}}); err != nil {
		t.Fatal(err)
	}
	notifyCommand(router, "/ls")
	if _, exists := router.notificationPreferences.Lookup(key); exists {
		t.Fatal("nonempty directory did not prune departed group")
	}
}

func TestRouterListPreservesOfflineAndUnsyncedMachinePreferences(t *testing.T) {
	router, _, _ := newRouterHarness(t)
	current := relaySession(1, "pane", "session", "task")
	offline, err := notificationprefs.NewIdentity("user-a", "offline", current.Display)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := notificationprefs.NewIdentity("user-a", "pending", current.Display)
	if err != nil {
		t.Fatal(err)
	}
	if err := router.notificationPreferences.SetBatch([]notificationprefs.Identity{offline, pending}, false); err != nil {
		t.Fatal(err)
	}
	attachSnapshot(t, router.catalog, "offline", ClientKey{UserID: "user-a", MachineID: "offline"}, hprp.SessionSnapshot{Sequence: 1, Sessions: []hprp.Session{current}})
	router.catalog.Detach("offline")
	if _, err := router.catalog.Attach("pending", ClientKey{UserID: "user-a", MachineID: "pending"}); err != nil {
		t.Fatal(err)
	}
	notifyCommand(router, "/ls")
	for _, key := range []notificationprefs.Identity{offline, pending} {
		if enabled, exists := router.notificationPreferences.Lookup(key); enabled || !exists {
			t.Fatal("offline or unsynced state lost")
		}
	}
	if err := router.catalog.ApplySnapshot("pending", hprp.SessionSnapshot{Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	notifyCommand(router, "/ls")
	if enabled, exists := router.notificationPreferences.Lookup(pending); enabled || !exists {
		t.Fatal("synced empty scope lost mute")
	}
	if err := router.catalog.ApplySnapshot("pending", hprp.SessionSnapshot{Sequence: 2, Sessions: []hprp.Session{relaySession(1, "new-pane", "new-session", "new group")}}); err != nil {
		t.Fatal(err)
	}
	notifyCommand(router, "/ls")
	if _, exists := router.notificationPreferences.Lookup(pending); exists {
		t.Fatal("synced nonempty scope not pruned")
	}
	if enabled, exists := router.notificationPreferences.Lookup(offline); enabled || !exists {
		t.Fatal("unrelated offline state pruned")
	}
}

func TestRouterFailedNotifyReplyKeepsPersistedState(t *testing.T) {
	router, gateway, _ := selectedRouterHarness(t)
	path := filepath.Join(t.TempDir(), "prefs.json")
	store, err := notificationprefs.LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	router.notificationPreferences = store
	entry, err := router.catalog.Selected("user-a")
	if err != nil {
		t.Fatal(err)
	}
	key, err := notificationIdentity("user-a", entry)
	if err != nil {
		t.Fatal(err)
	}
	gateway.FailNextResponse(errors.New("response lost"))
	notifyCommand(router, "/notify off")
	reloaded, err := notificationprefs.LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if enabled, exists := reloaded.Lookup(key); enabled || !exists {
		t.Fatal("reply failure rolled back persisted mute")
	}
	notifyCommand(router, "/ls")
	if !strings.Contains(gateway.LastReply(), "通知：✖️ 关闭") {
		t.Fatal(gateway.LastReply())
	}
}

func TestRouterListEmptyBackendPreservesReturningGroupMute(t *testing.T) {
	router, gateway, relay := selectedRouterHarness(t)
	entry, err := router.catalog.Selected("user-a")
	if err != nil {
		t.Fatal(err)
	}
	key, err := notificationIdentity("user-a", entry)
	if err != nil {
		t.Fatal(err)
	}
	notifyCommand(router, "/notify off")
	if err := router.catalog.ApplySnapshot("conn-1", hprp.SessionSnapshot{Sequence: 2}); err != nil {
		t.Fatal(err)
	}
	notifyCommand(router, "/ls")
	if enabled, exists := router.notificationPreferences.Lookup(key); enabled || !exists {
		t.Fatal("temporary empty directory lost mute")
	}
	// 临时空目录不清理，原组合恢复后继续保持关闭。
	if err := router.catalog.ApplySnapshot("conn-1", hprp.SessionSnapshot{Sequence: 3, Sessions: []hprp.Session{entry.Session}}); err != nil {
		t.Fatal(err)
	}
	before := gateway.ReplyCount()
	if err := router.SendNotification(context.Background(), "user-a", entry.Ref.MachineID, statusNotification(entry.Ref, "working", "done")); err != nil {
		t.Fatal(err)
	}
	if router.notificationPreferences.Enabled(key) || len(relay.FetchCalls()) != 0 || gateway.ReplyCount() != before {
		t.Fatal("recovering group fetched or delivered muted output")
	}
}
