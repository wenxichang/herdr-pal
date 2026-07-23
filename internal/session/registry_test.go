package session

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/herdr"
)

func TestReplaceBuildsSortedAgentTargets(t *testing.T) {
	registry := &Registry{}
	snapshot := testSnapshot(
		herdr.Pane{PaneID: "pane-z", TerminalID: "terminal-z", WorkspaceID: "workspace-b", TabID: "tab-b", Agent: stringPtr("codex"), DisplayAgent: stringPtr("Codex"), Title: stringPtr("Z"), AgentStatus: herdr.AgentStatusWorking},
		herdr.Pane{PaneID: "pane-a", TerminalID: "terminal-a", WorkspaceID: "workspace-a", TabID: "tab-a", Agent: stringPtr("claude"), DisplayAgent: stringPtr("Claude"), Title: stringPtr("A"), AgentStatus: herdr.AgentStatusIdle},
		herdr.Pane{PaneID: "not-an-agent", TerminalID: "terminal-none", WorkspaceID: "workspace-a", TabID: "tab-a"},
	)

	changes := registry.Replace(snapshot, false)
	if !changes.AgentSetChanged {
		t.Fatal("Replace() should report a changed agent set")
	}
	got := registry.CreateListSnapshot()
	if len(got) != 2 {
		t.Fatalf("CreateListSnapshot() returned %d targets, want 2", len(got))
	}
	if got[0].PaneID != "pane-a" || got[1].PaneID != "pane-z" {
		t.Fatalf("target order = %#v, want pane-a then pane-z", got)
	}
	if got[0].Workspace != "工作区 A" || got[0].Tab != "标签页 A" {
		t.Fatalf("target display hierarchy = %#v", got[0])
	}
}

func TestCreateListSnapshotSortsByWorkspaceAndTabNumbers(t *testing.T) {
	registry := &Registry{}
	snapshot := herdr.Snapshot{
		Workspaces: []herdr.Workspace{
			{WorkspaceID: "workspace-10", Number: 10, Label: "A"},
			{WorkspaceID: "workspace-2", Number: 2, Label: "Z"},
		},
		Tabs: []herdr.Tab{
			{TabID: "tab-10", WorkspaceID: "workspace-2", Number: 10, Label: "A"},
			{TabID: "tab-2", WorkspaceID: "workspace-2", Number: 2, Label: "Z"},
			{TabID: "tab-other", WorkspaceID: "workspace-10", Number: 1, Label: "A"},
		},
		Panes: []herdr.Pane{
			{PaneID: "pane-workspace-10", TerminalID: "terminal-3", WorkspaceID: "workspace-10", TabID: "tab-other", Agent: stringPtr("codex")},
			{PaneID: "pane-tab-10", TerminalID: "terminal-2", WorkspaceID: "workspace-2", TabID: "tab-10", Agent: stringPtr("codex")},
			{PaneID: "pane-tab-2", TerminalID: "terminal-1", WorkspaceID: "workspace-2", TabID: "tab-2", Agent: stringPtr("codex")},
		},
	}
	registry.Replace(snapshot, false)
	got := registry.CreateListSnapshot()
	if got[0].PaneID != "pane-tab-2" || got[1].PaneID != "pane-tab-10" || got[2].PaneID != "pane-workspace-10" {
		t.Fatalf("numeric target order = %#v", got)
	}
}

func TestSelectUsesMostRecentListSnapshot(t *testing.T) {
	registry := &Registry{}
	registry.Replace(testSnapshot(testAgentPane("pane-1", "terminal-1", "codex", nil)), false)
	if _, err := registry.Select(1); err == nil || !strings.Contains(err.Error(), "列表") {
		t.Fatalf("Select without list snapshot error = %v, want clear list error", err)
	}

	registry.CreateListSnapshot()
	selected, err := registry.Select(1)
	if err != nil {
		t.Fatalf("Select(1) error = %v", err)
	}
	if selected.PaneID != "pane-1" {
		t.Fatalf("Select(1) = %#v", selected)
	}
	if _, err := registry.Select(2); err == nil || !strings.Contains(err.Error(), "范围") {
		t.Fatalf("Select(2) error = %v, want range error", err)
	}
}

func TestReplaceInvalidatesSelectionForClosedOrReplacedOccupant(t *testing.T) {
	registry := &Registry{}
	original := testAgentPane("pane-1", "terminal-1", "codex", nil)
	registry.Replace(testSnapshot(original), false)
	registry.CreateListSnapshot()
	if _, err := registry.Select(1); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	changes := registry.Replace(testSnapshot(), false)
	if !changes.SelectionInvalidated || len(changes.RemovedTargets) != 1 {
		t.Fatalf("closed pane changes = %#v", changes)
	}
	if _, err := registry.ValidateSelected(); err == nil {
		t.Fatal("ValidateSelected() should reject a closed selected pane")
	}

	registry.Replace(testSnapshot(original), false)
	registry.CreateListSnapshot()
	if _, err := registry.Select(1); err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	replacement := testAgentPane("pane-1", "terminal-2", "claude", nil)
	changes = registry.Replace(testSnapshot(replacement), false)
	if !changes.SelectionInvalidated || len(changes.ReplacedTargets) != 1 {
		t.Fatalf("replaced pane changes = %#v", changes)
	}
	if _, err := registry.ValidateSelected(); err == nil {
		t.Fatal("ValidateSelected() should reject a replaced selected pane")
	}
}

func TestPaneReplacementInvalidatesSelectionEvenWhenOccupantKeyMatches(t *testing.T) {
	registry := &Registry{}
	session := &herdr.AgentSession{Source: "cli", Kind: "id", Value: "session-1"}
	registry.Replace(testSnapshot(testAgentPane("pane-old", "terminal-1", "codex", session)), false)
	registry.CreateListSnapshot()
	if _, err := registry.Select(1); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	changes := registry.Replace(testSnapshot(testAgentPane("pane-new", "terminal-1", "codex", session)), false)
	if !changes.SelectionInvalidated {
		t.Fatalf("pane replacement changes = %#v, want selection invalidated", changes)
	}
	if _, err := registry.ValidateSelected(); err == nil {
		t.Fatal("ValidateSelected() should reject a closed pane even if its occupant key reappears")
	}
	if _, err := registry.Select(1); err == nil {
		t.Fatal("Select() should reject a stale list entry for a closed pane")
	}
}

func TestReplacePreservingStatusKeepsOnlyMatchingOccupantStatus(t *testing.T) {
	registry := &Registry{}
	current := testAgentPane("pane-1", "terminal-1", "codex", &herdr.AgentSession{
		Source: "codex", Kind: "id", Value: "session-old",
	})
	current.AgentStatus = herdr.AgentStatusWorking
	registry.Replace(testSnapshot(current), false)

	sameOccupant := current
	sameOccupant.AgentStatus = herdr.AgentStatusDone
	sameOccupant.Title = stringPtr("更新后的标题")
	registry.ReplacePreservingStatus(testSnapshot(sameOccupant))
	targets := registry.CreateListSnapshot()
	if len(targets) != 1 || targets[0].Status != herdr.AgentStatusWorking || targets[0].Title != "更新后的标题" {
		t.Fatalf("同 occupant 结构替换未保留当前状态：%#v", targets)
	}

	replacement := sameOccupant
	replacement.AgentSession = &herdr.AgentSession{Source: "codex", Kind: "id", Value: "session-new"}
	replacement.AgentStatus = herdr.AgentStatusBlocked
	changes := registry.ReplacePreservingStatus(testSnapshot(replacement))
	targets = registry.CreateListSnapshot()
	if len(targets) != 1 || targets[0].Status != herdr.AgentStatusBlocked || len(changes.ReplacedTargets) != 1 {
		t.Fatalf("替换 occupant 错误保留旧状态：targets=%#v changes=%#v", targets, changes)
	}
}

func TestStatusChangeKeepsSelectionAndApplyStatusDoesNotCreateTargets(t *testing.T) {
	registry := &Registry{}
	pane := testAgentPane("pane-1", "terminal-1", "codex", nil)
	registry.Replace(testSnapshot(pane), false)
	registry.CreateListSnapshot()
	if _, err := registry.Select(1); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	pane.AgentStatus = herdr.AgentStatusWorking
	changes := registry.Replace(testSnapshot(pane), false)
	if changes.SelectionInvalidated || changes.AgentSetChanged {
		t.Fatalf("status-only change = %#v, want no identity change", changes)
	}
	selected, err := registry.ValidateSelected()
	if err != nil || selected.Status != herdr.AgentStatusWorking {
		t.Fatalf("ValidateSelected() = %#v, %v", selected, err)
	}

	transition, err := registry.ApplyStatus(herdr.AgentStatusEvent{PaneID: "pane-1", Agent: stringPtr("codex"), AgentStatus: herdr.AgentStatusDone})
	if err != nil || transition.Previous != herdr.AgentStatusWorking || transition.Current != herdr.AgentStatusDone {
		t.Fatalf("ApplyStatus() = %#v, %v", transition, err)
	}
	if _, err := registry.ApplyStatus(herdr.AgentStatusEvent{PaneID: "pane-1", Agent: stringPtr("codex"), AgentStatus: herdr.AgentStatusDone}); err != nil {
		t.Fatalf("repeated ApplyStatus() error = %v", err)
	}
	if _, err := registry.ApplyStatus(herdr.AgentStatusEvent{PaneID: "unknown", AgentStatus: herdr.AgentStatusIdle}); err == nil {
		t.Fatal("ApplyStatus() should reject unknown pane")
	}
	if len(registry.AgentPaneIDs()) != 1 {
		t.Fatalf("unknown event created a target: %v", registry.AgentPaneIDs())
	}
}

func TestApplyStatusRejectsStaleAgentEventAfterOccupantReplacement(t *testing.T) {
	registry := &Registry{}
	registry.Replace(testSnapshot(testAgentPane("pane-1", "terminal-1", "codex", nil)), false)
	replacement := testAgentPane("pane-1", "terminal-2", "claude", nil)
	replacement.AgentStatus = herdr.AgentStatusWorking
	registry.Replace(testSnapshot(replacement), false)

	if _, err := registry.ApplyStatus(herdr.AgentStatusEvent{PaneID: "pane-1", Agent: stringPtr("codex"), AgentStatus: herdr.AgentStatusDone}); !errors.Is(err, ErrStaleAgentEvent) {
		t.Fatalf("ApplyStatus() stale event error = %v, want ErrStaleAgentEvent", err)
	}
	current := registry.CreateListSnapshot()[0]
	if current.Status != herdr.AgentStatusWorking {
		t.Fatalf("stale event updated replacement status to %q", current.Status)
	}
	transition, err := registry.ApplyStatus(herdr.AgentStatusEvent{PaneID: "pane-1", Agent: stringPtr("claude"), AgentStatus: herdr.AgentStatusDone})
	if err != nil || transition.Previous != herdr.AgentStatusWorking || transition.Current != herdr.AgentStatusDone {
		t.Fatalf("ApplyStatus() matching event = %#v, %v", transition, err)
	}
	if _, err := registry.ApplyStatus(herdr.AgentStatusEvent{PaneID: "pane-1", AgentStatus: herdr.AgentStatusIdle}); !errors.Is(err, ErrStaleAgentEvent) {
		t.Fatalf("ApplyStatus() event without Agent error = %v, want ErrStaleAgentEvent", err)
	}
}

func TestRegistryErrorsAreClassifiable(t *testing.T) {
	registry := &Registry{}
	if _, err := registry.Select(1); !errors.Is(err, ErrNoListSnapshot) {
		t.Fatalf("Select() without snapshot error = %v", err)
	}
	registry.Replace(testSnapshot(testAgentPane("pane-1", "terminal-1", "codex", nil)), false)
	registry.CreateListSnapshot()
	if _, err := registry.Select(2); !errors.Is(err, ErrSelectionIndexOutOfRange) {
		t.Fatalf("Select() out of range error = %v", err)
	}
	if _, err := registry.ValidateSelected(); !errors.Is(err, ErrNoSelection) {
		t.Fatalf("ValidateSelected() without selection error = %v", err)
	}
	registry.Replace(testSnapshot(testAgentPane("pane-1", "terminal-2", "claude", nil)), false)
	if _, err := registry.Select(1); !errors.Is(err, ErrListSnapshotExpired) {
		t.Fatalf("Select() expired snapshot error = %v", err)
	}
	registry.CreateListSnapshot()
	if _, err := registry.Select(1); err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	registry.Replace(testSnapshot(), false)
	if _, err := registry.ValidateSelected(); !errors.Is(err, ErrSelectionInvalid) {
		t.Fatalf("ValidateSelected() invalid selection error = %v", err)
	}
	if _, err := registry.ApplyStatus(herdr.AgentStatusEvent{PaneID: "missing", Agent: stringPtr("codex")}); !errors.Is(err, ErrUnknownPane) {
		t.Fatalf("ApplyStatus() unknown pane error = %v", err)
	}
}

func TestReconnectClearsSelectionAndListSnapshot(t *testing.T) {
	registry := &Registry{}
	registry.Replace(testSnapshot(testAgentPane("pane-1", "terminal-1", "codex", nil)), false)
	registry.CreateListSnapshot()
	if _, err := registry.Select(1); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	changes := registry.Replace(testSnapshot(testAgentPane("pane-1", "terminal-1", "codex", nil)), true)
	if !changes.SelectionInvalidated {
		t.Fatalf("reconnect changes = %#v, want selection invalidated", changes)
	}
	if _, err := registry.ValidateSelected(); !errors.Is(err, ErrNoSelection) {
		t.Fatalf("ValidateSelected() after reconnect error = %v, want ErrNoSelection", err)
	}
	if _, err := registry.Select(1); !errors.Is(err, ErrNoListSnapshot) {
		t.Fatalf("Select() after reconnect error = %v, want ErrNoListSnapshot", err)
	}
}

func TestReconnectClearsPendingSelectionInvalidation(t *testing.T) {
	registry := &Registry{}
	original := testAgentPane("pane-1", "terminal-1", "codex", nil)
	registry.Replace(testSnapshot(original), false)
	registry.CreateListSnapshot()
	if _, err := registry.Select(1); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	registry.Replace(testSnapshot(testAgentPane("pane-1", "terminal-2", "claude", nil)), false)
	registry.Replace(testSnapshot(testAgentPane("pane-1", "terminal-2", "claude", nil)), true)
	if _, err := registry.ValidateSelected(); !errors.Is(err, ErrNoSelection) {
		t.Fatalf("ValidateSelected() after reconnect error = %v, want ErrNoSelection", err)
	}
	if _, err := registry.Select(1); !errors.Is(err, ErrNoListSnapshot) {
		t.Fatalf("Select() after reconnect error = %v, want ErrNoListSnapshot", err)
	}
}

func TestValidateSelectedReturnsCopy(t *testing.T) {
	registry := &Registry{}
	registry.Replace(testSnapshot(testAgentPane("pane-1", "terminal-1", "codex", nil)), false)
	registry.CreateListSnapshot()
	if _, err := registry.Select(1); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	first, err := registry.ValidateSelected()
	if err != nil {
		t.Fatalf("ValidateSelected() error = %v", err)
	}
	first.Title = "被外部改写"
	second, err := registry.ValidateSelected()
	if err != nil || second.Title == first.Title {
		t.Fatalf("target leaked internal state: %#v, %v", second, err)
	}
}

func TestOccupantKeyUsesStableSessionIdentity(t *testing.T) {
	registry := &Registry{}
	withSession := testAgentPane("pane-1", "terminal-1", "codex", &herdr.AgentSession{Source: "cli", Kind: "id", Value: "session-1"})
	registry.Replace(testSnapshot(withSession), false)
	first := registry.CreateListSnapshot()[0]
	if len(first.OccupantKey) != 64 {
		t.Fatalf("OccupantKey length = %d, want SHA-256 hex", len(first.OccupantKey))
	}
	withSession.AgentStatus = herdr.AgentStatusWorking
	changes := registry.Replace(testSnapshot(withSession), false)
	second := registry.CreateListSnapshot()[0]
	if first.OccupantKey != second.OccupantKey || changes.AgentSetChanged {
		t.Fatalf("status changed occupant identity: %#v", changes)
	}
	withSession.AgentSession.Value = "session-2"
	changes = registry.Replace(testSnapshot(withSession), false)
	third := registry.CreateListSnapshot()[0]
	if first.OccupantKey == third.OccupantKey || !changes.AgentSetChanged {
		t.Fatalf("session change did not replace occupant: %#v", changes)
	}
}

func TestOccupantKeyIgnoresAgentSessionAgentField(t *testing.T) {
	registry := &Registry{}
	pane := testAgentPane("pane-1", "terminal-1", "codex", &herdr.AgentSession{Source: "cli", Agent: "legacy-name", Kind: "id", Value: "session-1"})
	registry.Replace(testSnapshot(pane), false)
	first := registry.CreateListSnapshot()[0]
	pane.AgentSession.Agent = "renamed-but-same-session"
	changes := registry.Replace(testSnapshot(pane), false)
	second := registry.CreateListSnapshot()[0]
	if first.OccupantKey != second.OccupantKey || changes.AgentSetChanged {
		t.Fatalf("non-identity session field changed occupant: %#v", changes)
	}
}

func TestMatchesAgentVerifiesCurrentOccupant(t *testing.T) {
	registry := &Registry{}
	pane := testAgentPane("pane-1", "terminal-1", "codex", &herdr.AgentSession{Source: "cli", Kind: "id", Value: "session-1"})
	registry.Replace(testSnapshot(pane), false)
	target := registry.CreateListSnapshot()[0]
	current := herdr.AgentInfo{PaneID: "pane-1", TerminalID: "terminal-1", Agent: stringPtr("codex"), DisplayAgent: stringPtr("Codex"), AgentSession: &herdr.AgentSession{Source: "cli", Kind: "id", Value: "session-1"}}
	if !MatchesAgent(target, current) {
		t.Fatal("MatchesAgent() = false, want true")
	}
	current.AgentSession.Value = "session-2"
	if MatchesAgent(target, current) {
		t.Fatal("MatchesAgent() = true for a replacement occupant")
	}
}

func TestRegistryConcurrentSnapshotsAndReads(t *testing.T) {
	registry := &Registry{}
	registry.Replace(testSnapshot(testAgentPane("pane-1", "terminal-1", "codex", nil)), false)
	var group sync.WaitGroup
	for range 16 {
		group.Add(1)
		go func() {
			defer group.Done()
			registry.CreateListSnapshot()
			registry.AgentPaneIDs()
			registry.Replace(testSnapshot(testAgentPane("pane-1", "terminal-1", "codex", nil)), false)
		}()
	}
	group.Wait()
}

func testSnapshot(panes ...herdr.Pane) herdr.Snapshot {
	return herdr.Snapshot{
		Workspaces: []herdr.Workspace{
			{WorkspaceID: "workspace-a", Number: 1, Label: "工作区 A"},
			{WorkspaceID: "workspace-b", Number: 2, Label: "工作区 B"},
		},
		Tabs: []herdr.Tab{
			{TabID: "tab-a", WorkspaceID: "workspace-a", Number: 1, Label: "标签页 A"},
			{TabID: "tab-b", WorkspaceID: "workspace-b", Number: 1, Label: "标签页 B"},
		},
		Panes: panes,
	}
}

func testAgentPane(paneID, terminalID, agent string, session *herdr.AgentSession) herdr.Pane {
	return herdr.Pane{
		PaneID:       paneID,
		TerminalID:   terminalID,
		WorkspaceID:  "workspace-a",
		TabID:        "tab-a",
		Agent:        stringPtr(agent),
		DisplayAgent: stringPtr(strings.ToUpper(agent[:1]) + agent[1:]),
		Title:        stringPtr("标题 " + paneID),
		AgentStatus:  herdr.AgentStatusIdle,
		AgentSession: session,
	}
}

func stringPtr(value string) *string {
	return &value
}
