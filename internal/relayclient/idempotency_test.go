package relayclient

import (
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/hprp"
)

func TestCommandResultCacheReplaysEquivalentCommandAndRejectsConflict(t *testing.T) {
	now := time.Unix(100, 0)
	cache := newCommandResultCache(2, time.Minute, func() time.Time { return now })
	command := hprp.CommandExecute{
		IdempotencyKey: "message-1",
		Target:         hprp.Target{MachineID: "home", SlotID: "pane-1", SessionID: "session-1"},
		Content:        hprp.TextContent{Type: hprp.ContentTypeText, Text: "prompt"},
	}
	want := cachedCommandResult{
		result:  hprp.CommandResult{Outcome: hprp.OutcomeOK},
		outputs: []string{"part-1", "part-2"},
	}
	cache.Store(command, want)

	got, state := cache.Lookup(command)
	if state != commandCacheHit || got.result.Outcome != hprp.OutcomeOK || len(got.outputs) != 2 {
		t.Fatalf("Lookup(hit) = %#v, %v", got, state)
	}
	got.outputs[0] = "changed"
	replayed, _ := cache.Lookup(command)
	if replayed.outputs[0] != "part-1" {
		t.Fatal("cached outputs were not cloned")
	}

	conflict := command
	conflict.Content.Text = "different"
	if _, state := cache.Lookup(conflict); state != commandCacheConflict {
		t.Fatalf("Lookup(conflict) state = %v", state)
	}
	now = now.Add(time.Minute)
	if _, state := cache.Lookup(command); state != commandCacheMiss {
		t.Fatalf("Lookup(expired) state = %v", state)
	}
}
