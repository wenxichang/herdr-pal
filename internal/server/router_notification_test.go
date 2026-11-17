package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/notificationprefs"
)

var notificationTestSequence atomic.Uint64

func notifyCommand(router *ConversationRouter, content string) {
	message := routerMessage(fmt.Sprint("request-", notificationTestSequence.Add(1)), fmt.Sprint("message-", notificationTestSequence.Add(1)), "user-a", content)
	router.Handle(context.Background(), message)
}

func TestParseNotificationAction(t *testing.T) {
	for _, test := range []struct {
		input                  string
		count, issues          int
		all, explicit, invalid bool
	}{
		{input: "/notify"}, {input: "/notify off"},
		{input: "/notify on 1,2,3", count: 3, explicit: true},
		{input: "/notify off 1, 2 3，3", count: 3, explicit: true},
		{input: "/notify off 1,,2,", count: 2, explicit: true},
		{input: "/notify off 1,foo,0,-2,999999999999999999999999", count: 1, issues: 4, explicit: true},
		{input: "/notify on all", all: true},
		{input: "/notify off all,1", invalid: true},
		{input: "/notify off ,", invalid: true},
		{input: "/notify maybe", invalid: true},
		{input: "/notify off 1; /notify on 2", invalid: true},
		{input: "/1 /notify off", invalid: true}, {input: "#1 /notify on 2", invalid: true},
	} {
		t.Run(test.input, func(t *testing.T) {
			action, err := parseServerAction(test.input)
			if test.invalid {
				if err == nil {
					t.Fatalf("accepted %#v", action)
				}
				return
			}
			if err != nil || action.kind != serverActionNotify || action.notification == nil {
				t.Fatalf("action=%#v %v", action, err)
			}
			got := action.notification
			if len(got.indexes) != test.count || len(got.issues) != test.issues || got.all != test.all || got.hasExplicitIndexes != test.explicit {
				t.Fatalf("notification=%#v", got)
			}
		})
	}
}

func TestRouterNotificationBatchAndAll(t *testing.T) {
	router, gateway, relay, home, office := directedRouterHarness(t)
	notifyCommand(router, "/ls")
	notifyCommand(router, "/notify off 1,99,2")
	for _, entry := range []CatalogEntry{home, office} {
		key, _ := notificationIdentity("user-a", entry)
		if router.notificationPreferences.Enabled(key) {
			t.Fatal("valid group not disabled")
		}
	}
	if !strings.Contains(gateway.LastReply(), "99") || !strings.Contains(gateway.LastReply(), "2 个组合") {
		t.Fatal(gateway.LastReply())
	}
	before := relay.CallCount()
	notifyCommand(router, "/notify on all")
	if relay.CallCount() != before {
		t.Fatal("notify forwarded to Pal")
	}
	for _, entry := range []CatalogEntry{home, office} {
		key, _ := notificationIdentity("user-a", entry)
		if !router.notificationPreferences.Enabled(key) {
			t.Fatal("all did not enable")
		}
	}
	selected, _ := router.catalog.Selected("user-a")
	if selected.Ref != home.Ref {
		t.Fatal("selection changed")
	}
}

func TestRouterNotificationSharedGroupAndRename(t *testing.T) {
	router, gateway, _ := newRouterHarness(t)
	sessions := []hprp.Session{relaySession(1, "p1", "s1", "same"), relaySession(2, "p2", "s2", "same")}
	attachSnapshot(t, router.catalog, "a", ClientKey{UserID: "user-a", MachineID: "mac"}, hprp.SessionSnapshot{Sequence: 1, Sessions: sessions})
	notifyCommand(router, "/ls")
	if !strings.Contains(gateway.LastReply(), "共享开关") {
		t.Fatal(gateway.LastReply())
	}
	notifyCommand(router, "/notify off 1")
	key, _ := notificationIdentity("user-a", router.catalog.CatalogEntries("user-a")[1])
	if router.notificationPreferences.Enabled(key) {
		t.Fatal("shared group not muted")
	}
	_ = router.catalog.ApplySnapshot("a", hprp.SessionSnapshot{Sequence: 2, Sessions: []hprp.Session{relaySession(1, "p3", "s3", "same")}})
	notifyCommand(router, "/notify on 1,99")
	if !router.notificationPreferences.Enabled(key) {
		t.Fatal("replacement business identity not matched")
	}
}

func TestRouterListReconcilesNotificationPreferences(t *testing.T) {
	router, _, _ := selectedRouterHarness(t)
	notifyCommand(router, "/ls")
	notifyCommand(router, "/notify off")
	entry, _ := router.catalog.Selected("user-a")
	old, _ := notificationIdentity("user-a", entry)
	other, _ := notificationprefs.NewIdentity("other", entry.Ref.MachineID, entry.Session.Display)
	offline, _ := notificationprefs.NewIdentity("user-a", "offline", entry.Session.Display)
	_ = router.notificationPreferences.SetBatch([]notificationprefs.Identity{other, offline}, false)
	changed := entry.Session
	changed.Display.Title = "new title"
	_ = router.catalog.ApplySnapshot("conn-1", hprp.SessionSnapshot{Sequence: 2, Sessions: []hprp.Session{changed}})
	notifyCommand(router, "/ls ")
	if _, exists := router.notificationPreferences.Lookup(old); exists {
		t.Fatal("stale key retained")
	}
	next, _ := notificationprefs.NewIdentity("user-a", entry.Ref.MachineID, changed.Display)
	if enabled, exists := router.notificationPreferences.Lookup(next); !enabled || !exists {
		t.Fatal("new key not added on")
	}
	for _, key := range []notificationprefs.Identity{other, offline} {
		if enabled, exists := router.notificationPreferences.Lookup(key); enabled || !exists {
			t.Fatal("unrelated key removed")
		}
	}
}

func TestRouterListPreservesSyncedEmptyMachine(t *testing.T) {
	router, _, _ := selectedRouterHarness(t)
	notifyCommand(router, "/ls")
	notifyCommand(router, "/notify off")
	entry, _ := router.catalog.Selected("user-a")
	key, _ := notificationIdentity("user-a", entry)
	if err := router.catalog.ApplySnapshot("conn-1", hprp.SessionSnapshot{Sequence: 2}); err != nil {
		t.Fatal(err)
	}
	notifyCommand(router, "/ls ")
	if enabled, exists := router.notificationPreferences.Lookup(key); enabled || !exists {
		t.Fatal("empty machine lost mute")
	}
}

type failingNotificationStore struct{ *notificationprefs.Store }

func (store failingNotificationStore) SetBatch([]notificationprefs.Identity, bool) error {
	return errors.New("write failed")
}
func (store failingNotificationStore) Reconcile(string, []string, []notificationprefs.Identity) (notificationprefs.ReconcileResult, error) {
	return notificationprefs.ReconcileResult{}, errors.New("write failed")
}

func TestRouterListReconciliationFailureIsReported(t *testing.T) {
	router, gateway, _ := selectedRouterHarness(t)
	router.notificationPreferences = failingNotificationStore{notificationprefs.NewMemoryStore()}
	notifyCommand(router, "/ls")
	if !strings.Contains(gateway.LastReply(), "核对失败") {
		t.Fatal(gateway.LastReply())
	}
	if _, err := router.catalog.ResolveNotificationGroups("user-a", []int{1}); !errors.Is(err, ErrNoListSnapshot) {
		t.Fatal(err)
	}
}

func TestRouterListShowsNotificationPreferences(t *testing.T) {
	router, gateway, _ := selectedRouterHarness(t)
	notifyCommand(router, "/ls")
	if !strings.Contains(gateway.LastReply(), "通知：✔️ 开启") {
		t.Fatal(gateway.LastReply())
	}
	notifyCommand(router, "/notify off")
	notifyCommand(router, "/ls ")
	if !strings.Contains(gateway.LastReply(), "通知：✖️ 关闭") {
		t.Fatal(gateway.LastReply())
	}
}

func TestRouterListCommitsNotificationSnapshotOnlyAfterFullDelivery(t *testing.T) {
	for _, later := range []bool{false, true} {
		t.Run(fmt.Sprint(later), func(t *testing.T) {
			router, gateway, _ := newRouterHarness(t)
			var sessions []hprp.Session
			for i := range 100 {
				current := relaySession(i+1, fmt.Sprint("p", i), fmt.Sprint("s", i), strings.Repeat("long title ", 20))
				sessions = append(sessions, current)
			}
			attachSnapshot(t, router.catalog, "a", ClientKey{UserID: "user-a", MachineID: "mac"}, hprp.SessionSnapshot{Sequence: 1, Sessions: sessions})
			if later {
				gateway.FailNextSend(errors.New("send failed"))
			} else {
				gateway.FailNextResponse(errors.New("response failed"))
			}
			notifyCommand(router, "/ls")
			if _, err := router.catalog.ResolveNotificationGroups("user-a", []int{1}); !errors.Is(err, ErrNoListSnapshot) {
				t.Fatalf("err=%v", err)
			}
			firstKey, _ := notificationIdentity("user-a", router.catalog.CatalogEntries("user-a")[0])
			if _, exists := router.notificationPreferences.Lookup(firstKey); !exists {
				t.Fatal("delivery failure rolled back reconciliation")
			}
		})
	}
}

func TestRouterMutedNotificationHasNoSideEffects(t *testing.T) {
	for _, status := range []string{"done", "blocked", "working", "unknown", "idle"} {
		for _, image := range []bool{false, true} {
			t.Run(fmt.Sprint(status, image), func(t *testing.T) {
				router, gateway, relay := selectedRouterHarness(t)
				entry, _ := router.catalog.Selected("user-a")
				key, _ := notificationIdentity("user-a", entry)
				_ = router.notificationPreferences.SetBatch([]notificationprefs.Identity{key}, false)
				if image {
					_ = router.catalog.SetOutputMode("user-a", entry.Ref, hprp.OutputModeImage)
					relay.capabilities[hprp.CapabilityTerminalImageV1] = true
				}
				before := time.Now().Add(-time.Hour)
				router.activity.Touch("user-a", before)
				previous := "working"
				if status == "working" {
					previous = "idle"
				}
				if err := router.SendNotification(context.Background(), "user-a", entry.Ref.MachineID, statusNotification(entry.Ref, previous, status)); err != nil {
					t.Fatal(err)
				}
				if gateway.ReplyCount() != 0 || len(gateway.Images()) != 0 || len(relay.FetchCalls()) != 0 {
					t.Fatalf("leaked notification: replies=%v fetches=%v", gateway.Replies(), relay.FetchCalls())
				}
				if !router.activity.last["user-a"].Equal(before) {
					t.Fatal("muted event refreshed activity")
				}
			})
		}
	}
}

func TestRouterMutedInvalidationUsesRetiredIdentity(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			router, gateway, _ := selectedRouterHarness(t)
			entry, _ := router.catalog.Selected("user-a")
			key, _ := notificationIdentity("user-a", entry)
			_ = router.notificationPreferences.SetBatch([]notificationprefs.Identity{key}, enabled)
			_ = router.catalog.ApplySnapshot("conn-1", hprp.SessionSnapshot{Sequence: 2})
			if err := router.SendNotification(context.Background(), "user-a", entry.Ref.MachineID, invalidatedNotification(entry.Ref)); err != nil {
				t.Fatal(err)
			}
			if (gateway.ReplyCount() > 0) != enabled {
				t.Fatalf("enabled=%v messages=%v", enabled, gateway.Replies())
			}
		})
	}
}

func TestRouterPrunedGroupDoesNotEmitLateInvalidation(t *testing.T) {
	router, gateway, _ := selectedRouterHarness(t)
	entry, _ := router.catalog.Selected("user-a")
	notifyCommand(router, "/ls")
	notifyCommand(router, "/notify off")
	_ = router.catalog.ApplySnapshot("conn-1", hprp.SessionSnapshot{Sequence: 2, Sessions: []hprp.Session{relaySession(1, "other-pane", "other-session", "other group")}})
	notifyCommand(router, "/ls")
	before := gateway.ReplyCount()
	if err := router.SendNotification(context.Background(), "user-a", entry.Ref.MachineID, invalidatedNotification(entry.Ref)); err != nil {
		t.Fatal(err)
	}
	if gateway.ReplyCount() != before {
		t.Fatal("pruned group leaked late invalidation")
	}
	unknown := entry.Ref
	unknown.SessionID = "unknown"
	if err := router.SendNotification(context.Background(), "user-a", entry.Ref.MachineID, invalidatedNotification(unknown)); err != nil {
		t.Fatal(err)
	}
	if gateway.ReplyCount() != before {
		t.Fatal("unknown invalidation leaked")
	}
}

func TestRouterMutedGroupStillAnswersExplicitCommands(t *testing.T) {
	router, gateway, relay := selectedRouterHarness(t)
	entry, _ := router.catalog.Selected("user-a")
	relay.executeReply = RelayExecution{StructuredContent: terminalSnapshotResult(entry.Ref, hprp.OutputModeText, "explicit terminal", 1, 1).Content}
	notifyCommand(router, "/ls")
	notifyCommand(router, "/notify off")
	for _, command := range []string{"/ls", "/1", "/con", "/pageup", "/pagedn", "#1 /con", "prompt", "/key enter"} {
		before := gateway.ReplyCount()
		notifyCommand(router, command)
		if gateway.ReplyCount() <= before {
			t.Fatalf("command %s not answered", command)
		}
	}
	before := gateway.ReplyCount()
	if err := router.SendCommandOutput(context.Background(), "user-a", routerTerminalCommandOutput(entry.Ref, "followup", 1, 1)); err != nil {
		t.Fatal(err)
	}
	if gateway.ReplyCount() <= before || !strings.Contains(gateway.LastReply(), "followup") {
		t.Fatal("command output suppressed")
	}
	if len(relay.FetchCalls()) != 0 {
		t.Fatal("explicit commands used notification snapshot")
	}
}

func TestRouterReenableDoesNotReplayMutedEvents(t *testing.T) {
	router, gateway, relay := selectedRouterHarness(t)
	entry, _ := router.catalog.Selected("user-a")
	notifyCommand(router, "/notify off")
	if err := router.SendNotification(context.Background(), "user-a", entry.Ref.MachineID, statusNotification(entry.Ref, "working", "done")); err != nil {
		t.Fatal(err)
	}
	before := gateway.ReplyCount()
	notifyCommand(router, "/notify on")
	if gateway.ReplyCount() != before+1 || len(relay.FetchCalls()) != 0 {
		t.Fatal("toggle replayed muted notification")
	}
	if err := router.SendNotification(context.Background(), "user-a", entry.Ref.MachineID, statusNotification(entry.Ref, "working", "done")); err != nil {
		t.Fatal(err)
	}
	if len(relay.FetchCalls()) != 1 {
		t.Fatal("enabled event not fetched")
	}
}

func TestRouterNotificationToggleQueueOrdering(t *testing.T) {
	router, _, relay := selectedRouterHarness(t)
	entry, _ := router.catalog.Selected("user-a")
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = router.executor.Enqueue(ctx, "user-a", func(context.Context) error {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	<-started
	_ = router.executor.Enqueue(ctx, "user-a", func(ctx context.Context) error {
		return router.sendNotification(ctx, "user-a", entry.Ref.MachineID, statusNotification(entry.Ref, "working", "done"))
	})
	router.Dispatch(ctx, routerMessage("queue-off", "queue-off", "user-a", "/notify off"))
	_ = router.executor.Enqueue(ctx, "user-a", func(ctx context.Context) error {
		return router.sendNotification(ctx, "user-a", entry.Ref.MachineID, statusNotification(entry.Ref, "working", "done"))
	})
	_ = router.executor.Enqueue(ctx, "user-a", func(context.Context) error { close(done); return nil })
	close(release)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("queue timeout")
	}
	if len(relay.FetchCalls()) != 1 {
		t.Fatalf("fetches=%v", relay.FetchCalls())
	}
}

func TestRouterNotificationPartialAndAllInvalid(t *testing.T) {
	router, gateway, relay := selectedRouterHarness(t)
	entry, _ := router.catalog.Selected("user-a")
	key, _ := notificationIdentity("user-a", entry)
	notifyCommand(router, "/ls")
	before := relay.CallCount()
	notifyCommand(router, "/notify off foo,0,99")
	if !router.notificationPreferences.Enabled(key) || relay.CallCount() != before || !strings.Contains(gateway.LastReply(), "未修改") {
		t.Fatal("all invalid inputs changed current selection")
	}
	notifyCommand(router, "/notify off foo,1,99")
	if router.notificationPreferences.Enabled(key) || !strings.Contains(gateway.LastReply(), "跳过 foo") || !strings.Contains(gateway.LastReply(), "跳过 99") {
		t.Fatal(gateway.LastReply())
	}
}

func TestRouterNotificationSaveFailureAndDuplicateWebhook(t *testing.T) {
	router, gateway, _ := selectedRouterHarness(t)
	entry, _ := router.catalog.Selected("user-a")
	key, _ := notificationIdentity("user-a", entry)
	notifyCommand(router, "/ls")
	memory := router.notificationPreferences.(*notificationprefs.Store)
	router.notificationPreferences = failingNotificationStore{memory}
	notifyCommand(router, "/notify off 1,99")
	if !memory.Enabled(key) || !strings.Contains(gateway.LastReply(), "均未生效") {
		t.Fatal(gateway.LastReply())
	}
	router.notificationPreferences = memory
	message := routerMessage("dedupe-off", "dedupe-notify", "user-a", "/notify off")
	router.Handle(context.Background(), message)
	message.Content = "/notify on"
	router.Handle(context.Background(), message)
	if memory.Enabled(key) {
		t.Fatal("duplicate webhook mutated preference twice")
	}
}

func TestRouterMutedBackgroundNoticeAndImageFallback(t *testing.T) {
	router, gateway, relay, _, office := directedRouterHarness(t)
	notifyCommand(router, "/ls")
	key, _ := notificationIdentity("user-a", office)
	_ = router.notificationPreferences.SetBatch([]notificationprefs.Identity{key}, false)
	before := gateway.ReplyCount()
	if err := router.SendNotification(context.Background(), "user-a", office.Ref.MachineID, statusNotification(office.Ref, "working", "done")); err != nil {
		t.Fatal(err)
	}
	if gateway.ReplyCount() != before || len(relay.FetchCalls()) != 0 {
		t.Fatal("muted background sent brief notice")
	}
	router.gateway = notificationImageFallbackGateway{gateway}
	relay.capabilities[hprp.CapabilityTerminalImageV1] = true
	_ = router.catalog.SetOutputMode("user-a", office.Ref, hprp.OutputModeImage)
	content := terminalImageContent("explicit fallback text", 1, 1)
	relay.executeReply = RelayExecution{StructuredContent: &content}
	notifyCommand(router, "#2 /con")
	if relay.CallCount() == 0 || !strings.Contains(gateway.LastReply(), "explicit fallback text") {
		t.Fatal("explicit image query fallback suppressed")
	}
}

type notificationImageFallbackGateway struct{ *routerGateway }

func (gateway notificationImageFallbackGateway) SendImageTo(context.Context, string, []byte) error {
	return errors.New("image failed")
}

func TestNotificationStateLabelEmoji(t *testing.T) {
	for _, test := range []struct {
		enabled bool
		want    string
	}{{true, "✔️ 开启"}, {false, "✖️ 关闭"}} {
		if got := notificationStateLabel(test.enabled); got != test.want {
			t.Fatalf("enabled=%v label=%q want=%q", test.enabled, got, test.want)
		}
	}
}

func TestRouterNotifyQueryShowsStateEmoji(t *testing.T) {
	router, gateway, _ := selectedRouterHarness(t)
	notifyCommand(router, "/notify")
	if !strings.Contains(gateway.LastReply(), "通知：✔️ 开启") {
		t.Fatal(gateway.LastReply())
	}
	notifyCommand(router, "/notify off")
	notifyCommand(router, "/notify")
	if !strings.Contains(gateway.LastReply(), "通知：✖️ 关闭") {
		t.Fatal(gateway.LastReply())
	}
}
