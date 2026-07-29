package relayclient

import (
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"

	"github.com/wenxichang/herdr-pal/internal/hprp"
)

type commandCacheState uint8

const (
	commandCacheMiss commandCacheState = iota
	commandCacheHit
	commandCacheConflict
	commandCacheFull
)

type cachedCommandResult struct {
	target       hprp.Target
	fingerprint  [sha256.Size]byte
	result       hprp.CommandResult
	outputs      []hprp.Content
	expiresAt    time.Time
	payloadBytes int
}

type commandResultCache struct {
	mu        sync.Mutex
	entries   map[string]cachedCommandResult
	capacity  int
	maxBytes  int
	usedBytes int
	ttl       time.Duration
	now       func() time.Time
}

func newCommandResultCache(capacity, maxBytes int, ttl time.Duration, now func() time.Time) *commandResultCache {
	if capacity <= 0 {
		capacity = 1
	}
	if maxBytes <= 0 {
		maxBytes = 1
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	if now == nil {
		now = time.Now
	}
	return &commandResultCache{entries: make(map[string]cachedCommandResult), capacity: capacity, maxBytes: maxBytes, ttl: ttl, now: now}
}

func (cache *commandResultCache) Lookup(command hprp.CommandExecute) (cachedCommandResult, commandCacheState) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.pruneLocked(cache.now())
	entry, exists := cache.entries[command.IdempotencyKey]
	if !exists {
		if len(cache.entries) >= cache.capacity {
			return cachedCommandResult{}, commandCacheFull
		}
		return cachedCommandResult{}, commandCacheMiss
	}
	if entry.fingerprint != commandFingerprint(command) {
		return cachedCommandResult{}, commandCacheConflict
	}
	return cloneCachedCommandResult(entry), commandCacheHit
}

func (cache *commandResultCache) Store(command hprp.CommandExecute, result cachedCommandResult) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.pruneLocked(cache.now())
	if existing, exists := cache.entries[command.IdempotencyKey]; exists {
		cache.usedBytes -= existing.payloadBytes
	} else if len(cache.entries) >= cache.capacity {
		return
	}

	result = cloneCachedCommandResult(result)
	result.target = command.Target
	result.fingerprint = commandFingerprint(command)
	result.expiresAt = cache.now().Add(cache.ttl)
	result = cache.fitPayloadLocked(result)
	cache.entries[command.IdempotencyKey] = result
	cache.usedBytes += result.payloadBytes
}

func (cache *commandResultCache) fitPayloadLocked(entry cachedCommandResult) cachedCommandResult {
	remaining := cache.maxBytes - cache.usedBytes
	entry.payloadBytes = cachedPayloadSize(entry)
	if entry.payloadBytes <= remaining {
		return entry
	}
	entry.outputs = nil
	entry.payloadBytes = cachedPayloadSize(entry)
	if entry.payloadBytes <= remaining {
		return entry
	}
	entry.result.Content = nil
	if entry.result.Error != nil {
		entry.result.Error.Message = ""
		entry.result.Error.Details = nil
	}
	entry.payloadBytes = 0
	return entry
}

func (cache *commandResultCache) pruneLocked(now time.Time) {
	for key, entry := range cache.entries {
		if !now.Before(entry.expiresAt) {
			cache.usedBytes -= entry.payloadBytes
			delete(cache.entries, key)
		}
	}
}

func commandFingerprint(command hprp.CommandExecute) [sha256.Size]byte {
	encoded, _ := json.Marshal(struct {
		Target     hprp.Target      `json:"target"`
		Content    hprp.TextContent `json:"content"`
		OutputMode hprp.OutputMode  `json:"output_mode"`
	}{Target: command.Target, Content: command.Content, OutputMode: command.OutputMode})
	return sha256.Sum256(encoded)
}

func cachedPayloadSize(entry cachedCommandResult) int {
	encoded, err := json.Marshal(struct {
		Content *hprp.Content  `json:"content,omitempty"`
		Error   *hprp.Error    `json:"error,omitempty"`
		Outputs []hprp.Content `json:"outputs,omitempty"`
	}{Content: entry.result.Content, Error: entry.result.Error, Outputs: entry.outputs})
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return len(encoded)
}

func cloneCachedCommandResult(entry cachedCommandResult) cachedCommandResult {
	entry.outputs = cloneHPRPContents(entry.outputs)
	if entry.result.Content != nil {
		content := cloneHPRPContent(*entry.result.Content)
		entry.result.Content = &content
	}
	if entry.result.ReplacementTarget != nil {
		target := *entry.result.ReplacementTarget
		entry.result.ReplacementTarget = &target
	}
	if entry.result.Error != nil {
		resultError := *entry.result.Error
		if entry.result.Error.Details != nil {
			resultError.Details = make(map[string]any, len(entry.result.Error.Details))
			for key, value := range entry.result.Error.Details {
				resultError.Details[key] = value
			}
		}
		entry.result.Error = &resultError
	}
	return entry
}
