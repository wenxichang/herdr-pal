package relayclient

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/session"
)

func TestBuildSnapshotPreservesStableTargetAndDisplayMetadata(t *testing.T) {
	targets := []session.Target{{
		PaneID: "pane-1", TerminalID: "terminal-1", OccupantKey: "occ-1", Agent: "codex",
		DisplayAgent: "Codex", Title: "实现 Relay", Status: herdr.AgentStatusWorking, Workspace: "herdr-pal", Tab: "main",
	}}
	snapshot := BuildSnapshot(3, targets)
	if snapshot.Sequence != 3 || len(snapshot.Sessions) != 1 {
		t.Fatalf("BuildSnapshot() = %#v", snapshot)
	}
	current := snapshot.Sessions[0]
	if current.Display.Index != 1 || current.SlotID != "pane-1" || current.SessionID != "occ-1" || current.Display.Title != "实现 Relay" || current.Status != "working" {
		t.Fatalf("session = %#v", current)
	}
}

func TestSnapshotFingerprintIgnoresSequenceButDetectsMetadataChange(t *testing.T) {
	first := BuildSnapshot(1, []session.Target{{PaneID: "pane-1", TerminalID: "terminal-1", OccupantKey: "occ-1", Title: "old", Status: herdr.AgentStatusIdle}})
	second := first
	second.Sequence = 2
	if SnapshotFingerprint(first) != SnapshotFingerprint(second) {
		t.Fatal("sequence changed fingerprint")
	}
	second.Sessions = append([]hprp.Session(nil), first.Sessions...)
	second.Sessions[0].Display.Title = "new"
	if SnapshotFingerprint(first) == SnapshotFingerprint(second) {
		t.Fatal("metadata change did not change fingerprint")
	}
}

func TestBuildSnapshotTruncatesOversizedLabelsOnUTF8Boundary(t *testing.T) {
	targets := []session.Target{{
		PaneID: "dsh:session-1", OccupantKey: "session:1", Agent: "dsh", DisplayAgent: "DeepSeek Harness",
		Title: strings.Repeat("中", 256), Status: herdr.AgentStatusIdle,
		Workspace: strings.Repeat("🙂", 256), Tab: "web",
	}}

	snapshot := BuildSnapshot(1, targets)
	if err := hprp.ValidateSessionSnapshot(snapshot); err != nil {
		t.Fatalf("ValidateSessionSnapshot() 返回错误：%v", err)
	}
	current := snapshot.Sessions[0]
	if len(current.Display.Title) > hprp.MaxLabelBytes || !utf8.ValidString(current.Display.Title) {
		t.Fatalf("Title 长度 = %d（%q），必须 <= %d 且 UTF-8 有效", len(current.Display.Title), current.Display.Title, hprp.MaxLabelBytes)
	}
	if len(current.Display.Workspace) > hprp.MaxLabelBytes || !utf8.ValidString(current.Display.Workspace) {
		t.Fatalf("Workspace 长度 = %d（%q），必须 <= %d 且 UTF-8 有效", len(current.Display.Workspace), current.Display.Workspace, hprp.MaxLabelBytes)
	}
	if current.Display.Title != strings.Repeat("中", hprp.MaxLabelBytes/3) ||
		current.Display.Workspace != strings.Repeat("🙂", hprp.MaxLabelBytes/4) {
		t.Fatalf("截断结果 = (%q, %q)，want 完整 UTF-8 边界", current.Display.Title, current.Display.Workspace)
	}
}
