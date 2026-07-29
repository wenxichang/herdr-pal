package relayclient

import (
	"sync"
	"time"

	"github.com/wenxichang/herdr-pal/internal/hprp"
)

type commandCacheState uint8

const (
	commandCacheMiss commandCacheState = iota
	commandCacheHit
	commandCacheConflict
)

type cachedCommandResult struct {
	command   hprp.CommandExecute
	result    hprp.CommandResult
	outputs   []hprp.Content
	expiresAt time.Time
	createdAt time.Time
}

type commandResultCache struct {
	mu       sync.Mutex
	entries  map[string]cachedCommandResult
	capacity int
	ttl      time.Duration
	now      func() time.Time
}

func newCommandResultCache(capacity int, ttl time.Duration, now func() time.Time) *commandResultCache {
	if capacity <= 0 {
		capacity = 1
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	if now == nil {
		now = time.Now
	}
	return &commandResultCache{entries: make(map[string]cachedCommandResult), capacity: capacity, ttl: ttl, now: now}
}

func (cache *commandResultCache) Lookup(command hprp.CommandExecute) (cachedCommandResult, commandCacheState) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := cache.now()
	cache.pruneLocked(now)
	entry, exists := cache.entries[command.IdempotencyKey]
	if !exists {
		return cachedCommandResult{}, commandCacheMiss
	}
	if entry.command.Target != command.Target || entry.command.Content != command.Content || entry.command.OutputMode != command.OutputMode {
		return cachedCommandResult{}, commandCacheConflict
	}
	return cloneCachedCommandResult(entry), commandCacheHit
}

func (cache *commandResultCache) Store(command hprp.CommandExecute, result cachedCommandResult) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := cache.now()
	cache.pruneLocked(now)
	if len(cache.entries) >= cache.capacity {
		cache.evictOldestLocked()
	}
	result = cloneCachedCommandResult(result)
	result.command = command
	result.createdAt = now
	result.expiresAt = now.Add(cache.ttl)
	cache.entries[command.IdempotencyKey] = result
}

func (cache *commandResultCache) pruneLocked(now time.Time) {
	for key, entry := range cache.entries {
		if !now.Before(entry.expiresAt) {
			delete(cache.entries, key)
		}
	}
}

func (cache *commandResultCache) evictOldestLocked() {
	var oldestKey string
	var oldestTime time.Time
	for key, entry := range cache.entries {
		if oldestKey == "" || entry.createdAt.Before(oldestTime) {
			oldestKey = key
			oldestTime = entry.createdAt
		}
	}
	delete(cache.entries, oldestKey)
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
	return entry
}
