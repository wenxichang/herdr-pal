package relayclient

import (
	"strings"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/hprp"
)

func TestCommandResultCacheReplaysEquivalentCommandAndRejectsConflict(t *testing.T) {
	now := time.Unix(100, 0)
	cache := newCommandResultCache(2, 1<<20, time.Minute, func() time.Time { return now })
	command := hprp.CommandExecute{
		IdempotencyKey: "message-1",
		Target:         hprp.Target{MachineID: "home", SlotID: "pane-1", SessionID: "session-1"},
		Content:        hprp.TextContent{Type: hprp.ContentTypeText, Text: "prompt"},
		OutputMode:     hprp.OutputModeText,
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

func TestCommandResultCacheKeepsAllKeysAndCompactsNewPayloadToRespectByteBudget(t *testing.T) {
	now := time.Unix(100, 0)
	cache := newCommandResultCache(10, 500, time.Minute, func() time.Time { return now })
	first := testCachedCommand("message-1", "first")
	second := testCachedCommand("message-2", "second")
	cache.Store(first, cachedCommandResult{result: hprp.CommandResult{Outcome: hprp.OutcomeOK}, outputs: []hprp.Content{{Type: hprp.ContentTypeText, Text: strings.Repeat("x", 300)}}})
	now = now.Add(time.Second)
	cache.Store(second, cachedCommandResult{result: hprp.CommandResult{Outcome: hprp.OutcomeOK}, outputs: []hprp.Content{{Type: hprp.ContentTypeText, Text: strings.Repeat("x", 300)}}})

	firstResult, state := cache.Lookup(first)
	if state != commandCacheHit || len(firstResult.outputs) != 1 {
		t.Fatalf("Lookup(first) = %#v, %v, want complete hit", firstResult, state)
	}
	secondResult, state := cache.Lookup(second)
	if state != commandCacheHit || len(secondResult.outputs) != 0 {
		t.Fatalf("Lookup(second) = %#v, %v, want compacted hit", secondResult, state)
	}
	if cache.usedBytes > cache.maxBytes {
		t.Fatalf("usedBytes = %d, maxBytes = %d", cache.usedBytes, cache.maxBytes)
	}
}

func TestCommandResultCacheRejectsNewKeysAtCapacityWithoutEvictingProtectedKeys(t *testing.T) {
	cache := newCommandResultCache(1, 1<<20, time.Minute, time.Now)
	first := testCachedCommand("message-1", "first")
	second := testCachedCommand("message-2", "second")
	cache.Store(first, cachedCommandResult{result: hprp.CommandResult{Outcome: hprp.OutcomeOK}})

	if _, state := cache.Lookup(second); state != commandCacheFull {
		t.Fatalf("Lookup(second) state = %v, want full", state)
	}
	if _, state := cache.Lookup(first); state != commandCacheHit {
		t.Fatalf("Lookup(first) state = %v, want hit", state)
	}
}

func TestCommandResultCacheKeepsResultButDropsOversizedSupplementalOutputs(t *testing.T) {
	cache := newCommandResultCache(10, 512, time.Minute, time.Now)
	command := testCachedCommand("message-1", "prompt")
	cache.Store(command, cachedCommandResult{
		result:  hprp.CommandResult{Outcome: hprp.OutcomeOK, Content: &hprp.Content{Type: hprp.ContentTypeText, Text: "accepted"}},
		outputs: []hprp.Content{{Type: hprp.ContentTypeText, Text: strings.Repeat("x", 1024)}},
	})

	got, state := cache.Lookup(command)
	if state != commandCacheHit {
		t.Fatalf("Lookup() state = %v, want hit", state)
	}
	if got.result.Content == nil || got.result.Content.Text != "accepted" {
		t.Fatalf("Lookup() result = %#v", got.result)
	}
	if len(got.outputs) != 0 {
		t.Fatalf("Lookup() outputs = %d, want 0", len(got.outputs))
	}
}

func testCachedCommand(idempotencyKey, text string) hprp.CommandExecute {
	return hprp.CommandExecute{
		IdempotencyKey: idempotencyKey,
		Target:         hprp.Target{MachineID: "home", SlotID: "pane-1", SessionID: "session-1"},
		Content:        hprp.TextContent{Type: hprp.ContentTypeText, Text: text},
		OutputMode:     hprp.OutputModeText,
	}
}
