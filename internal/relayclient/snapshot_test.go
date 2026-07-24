package relayclient

import (
	"testing"

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/relayproto"
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
	if current.LocalIndex != 1 || current.PaneID != "pane-1" || current.OccupantHash != "occ-1" || current.Title != "实现 Relay" || current.Status != "working" {
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
	second.Sessions = append([]relayproto.Session(nil), first.Sessions...)
	second.Sessions[0].Title = "new"
	if SnapshotFingerprint(first) == SnapshotFingerprint(second) {
		t.Fatal("metadata change did not change fingerprint")
	}
}
