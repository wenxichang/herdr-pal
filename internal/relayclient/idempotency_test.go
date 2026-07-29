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
	capturedAt := now.UTC()
	want := cachedCommandResult{
		result: hprp.CommandResult{Outcome: hprp.OutcomeOK, Content: &hprp.Content{
			Type: hprp.ContentTypeTerminal, Text: "result", Mode: hprp.OutputModeText, CapturedAt: &capturedAt,
		}},
		outputs: []hprp.Content{
			{
				Type: hprp.ContentTypeTerminal, Text: "part-1", Mode: hprp.OutputModeImage,
				Image: &hprp.TerminalImage{MediaType: "image/png", Encoding: "base64", Data: "data", Width: 2, Height: 1, ColorMode: "indexed-256"},
				Page:  &hprp.TerminalPage{Current: 1, Total: 1}, CapturedAt: &capturedAt,
			},
			{Type: hprp.ContentTypeText, Text: "part-2"},
		},
	}
	cache.Store(command, want)

	got, state := cache.Lookup(command)
	if state != commandCacheHit || got.result.Outcome != hprp.OutcomeOK || len(got.outputs) != 2 {
		t.Fatalf("Lookup(hit) = %#v, %v", got, state)
	}
	got.outputs[0].Text = "changed"
	got.outputs[0].Image.Data = "changed"
	got.result.Content.Text = "changed"
	replayed, _ := cache.Lookup(command)
	if replayed.outputs[0].Text != "part-1" || replayed.outputs[0].Image.Data != "data" || replayed.result.Content.Text != "result" {
		t.Fatal("cached outputs were not cloned")
	}

	conflict := command
	conflict.Content.Text = "different"
	if _, state := cache.Lookup(conflict); state != commandCacheConflict {
		t.Fatalf("Lookup(conflict) state = %v", state)
	}
	modeConflict := command
	modeConflict.OutputMode = hprp.OutputModeImage
	if _, state := cache.Lookup(modeConflict); state != commandCacheConflict {
		t.Fatalf("Lookup(mode conflict) state = %v", state)
	}
	now = now.Add(time.Minute)
	if _, state := cache.Lookup(command); state != commandCacheMiss {
		t.Fatalf("Lookup(expired) state = %v", state)
	}
}
