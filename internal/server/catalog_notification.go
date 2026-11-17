package server

import (
	"sort"
	"strconv"
	"time"

	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/notificationprefs"
)

const retiredNotificationTTL = 10 * time.Minute
const retiredNotificationCapacity = 4096

// NotificationInventory 是同次目录视图的已同步在线机器和全部会话。
type NotificationInventory struct {
	MachineIDs []string
	Entries    []CatalogEntry
}

// NotificationIndexIssue 描述批量操作跳过的单项输入及原因。
type NotificationIndexIssue struct {
	Input  string
	Reason string
}

// NotificationBatchResolution 是按已展示组合逐项匹配后的有效范围。
type NotificationBatchResolution struct {
	Identities       []notificationprefs.Identity
	AffectedSessions int
	Skipped          []NotificationIndexIssue
}

// NotificationSource 区分可操作在线来源与仅可解释失效通知的退出来源。
type NotificationSource struct {
	Entry   CatalogEntry
	Retired bool
}

type notificationSourceKey struct {
	userID string
	target hprp.Target
}
type retiredNotificationSource struct {
	entry CatalogEntry
	at    time.Time
}

// NotificationInventory 一次捕获已同步机器及其目录，清理时按机器判断列表是否非空。
func (catalog *SessionCatalog) NotificationInventory(userID string) NotificationInventory {
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	result := NotificationInventory{Entries: cloneEntries(catalog.entriesLocked(userID))}
	for key, machine := range catalog.machines {
		if key.UserID == userID && machine.sequence > 0 {
			result.MachineIDs = append(result.MachineIDs, key.MachineID)
		}
	}
	sort.Strings(result.MachineIDs)
	return result
}

// CommitNotificationList 保存完整送达的编号到业务组合映射，与自动编号相互独立。
func (catalog *SessionCatalog) CommitNotificationList(userID string, entries []CatalogEntry) error {
	keys := make([]notificationprefs.Identity, len(entries))
	for i, entry := range entries {
		key, err := notificationIdentity(userID, entry)
		if err != nil {
			return err
		}
		keys[i] = key
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	catalog.notificationLists[userID] = keys
	return nil
}

// InvalidateNotificationList 使未完整送达的新列表无法用于批量设置。
func (catalog *SessionCatalog) InvalidateNotificationList(userID string) {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	delete(catalog.notificationLists, userID)
}

// ResolveNotificationGroups 按最近送达列表匹配当前组合，单项失效不影响其他项。
func (catalog *SessionCatalog) ResolveNotificationGroups(userID string, indexes []int) (NotificationBatchResolution, error) {
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	list, exists := catalog.notificationLists[userID]
	if !exists {
		return NotificationBatchResolution{}, ErrNoListSnapshot
	}
	counts := make(map[notificationprefs.Identity]int)
	for _, entry := range catalog.entriesLocked(userID) {
		key, err := notificationIdentity(userID, entry)
		if err != nil {
			return NotificationBatchResolution{}, err
		}
		counts[key]++
	}
	result := NotificationBatchResolution{}
	seen := make(map[notificationprefs.Identity]bool)
	for _, index := range indexes {
		reason := ""
		if index < 1 || index > len(list) {
			reason = "编号不存在"
		} else if counts[list[index-1]] == 0 {
			reason = "组合已失效或机器离线"
		}
		if reason != "" {
			result.Skipped = append(result.Skipped, NotificationIndexIssue{Input: strconv.Itoa(index), Reason: reason})
			continue
		}
		key := list[index-1]
		if !seen[key] {
			seen[key] = true
			result.Identities = append(result.Identities, key)
			result.AffectedSessions += counts[key]
		}
	}
	return result, nil
}

func notificationIdentity(userID string, entry CatalogEntry) (notificationprefs.Identity, error) {
	return notificationprefs.NewIdentity(userID, entry.Ref.MachineID, entry.Session.Display)
}

func (catalog *SessionCatalog) createNumberedSnapshotFromEntries(userID string, entries []CatalogEntry) []CatalogEntry {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	catalog.setNumberedEntriesLocked(userID, entries)
	return cloneEntries(entries)
}

func (catalog *SessionCatalog) setNumberedEntriesLocked(userID string, entries []CatalogEntry) {
	routing := catalog.routing[userID]
	routing.numbered = make([]hprp.Target, len(entries))
	for i, entry := range entries {
		routing.numbered[i] = entry.Ref
	}
	routing.numberedValid = true
	catalog.routing[userID] = routing
}

// ResolveNotificationSource 复核在线目标；仅失效事件允许回查有界退出缓存。
func (catalog *SessionCatalog) ResolveNotificationSource(userID string, target hprp.Target, allowRetired bool) (NotificationSource, error) {
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	if entry, exists := catalog.findEntryLocked(userID, target); exists {
		return NotificationSource{Entry: entry}, nil
	}
	if allowRetired {
		source, exists := catalog.retiredNotifications[notificationSourceKey{userID: userID, target: target}]
		if exists && catalog.notificationNow().Before(source.at.Add(retiredNotificationTTL)) {
			return NotificationSource{Entry: source.entry, Retired: true}, nil
		}
	}
	return NotificationSource{}, ErrTargetChanged
}

func (catalog *SessionCatalog) recordRetiredNotificationSourcesLocked(key ClientKey, previous, next []hprp.Session) {
	now := catalog.notificationNow()
	active := make(map[hprp.Target]bool, len(next))
	for _, current := range next {
		target := hprp.Target{MachineID: key.MachineID, SlotID: current.SlotID, SessionID: current.SessionID}
		active[target] = true
		delete(catalog.retiredNotifications, notificationSourceKey{userID: key.UserID, target: target})
	}
	for _, current := range previous {
		target := hprp.Target{MachineID: key.MachineID, SlotID: current.SlotID, SessionID: current.SessionID}
		if !active[target] {
			catalog.retiredNotifications[notificationSourceKey{userID: key.UserID, target: target}] = retiredNotificationSource{entry: CatalogEntry{Ref: target, Session: current}, at: now}
		}
	}
	for sourceKey, source := range catalog.retiredNotifications {
		if !now.Before(source.at.Add(retiredNotificationTTL)) {
			delete(catalog.retiredNotifications, sourceKey)
		}
	}
	for len(catalog.retiredNotifications) > retiredNotificationCapacity {
		var oldestKey notificationSourceKey
		var oldest time.Time
		for sourceKey, source := range catalog.retiredNotifications {
			if oldest.IsZero() || source.at.Before(oldest) {
				oldest = source.at
				oldestKey = sourceKey
			}
		}
		delete(catalog.retiredNotifications, oldestKey)
	}
}

// ForgetRetiredNotificationGroups 删除已清理组合的退出来源，防止迟到事件恢复默认开启。
func (catalog *SessionCatalog) ForgetRetiredNotificationGroups(userID string, keys []notificationprefs.Identity) {
	if len(keys) == 0 {
		return
	}
	removed := make(map[notificationprefs.Identity]bool, len(keys))
	for _, key := range keys {
		removed[key] = true
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	for sourceKey, source := range catalog.retiredNotifications {
		if sourceKey.userID != userID {
			continue
		}
		key, err := notificationIdentity(userID, source.entry)
		if err == nil && removed[key] {
			delete(catalog.retiredNotifications, sourceKey)
		}
	}
}
