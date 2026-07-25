// Package server 实现中央 Relay Server 的内存目录、路由和连接管理。
package server

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/wenxichang/herdr-pal/internal/relayproto"
)

var (
	// ErrDuplicateClient 表示同一 userid 与 machine_id 已有在线连接。
	ErrDuplicateClient = errors.New("Relay 客户端已连接")
	// ErrUnknownConnection 表示数据来自已关闭或未知连接。
	ErrUnknownConnection = errors.New("Relay 连接不存在")
	// ErrSnapshotStale 表示快照序号没有递增。
	ErrSnapshotStale = errors.New("Relay 快照已过期")
	// ErrNoListSnapshot 表示用户尚未生成可选择编号。
	ErrNoListSnapshot = errors.New("尚无会话列表")
	// ErrSelectionIndexOutOfRange 表示全局编号不在列表范围内。
	ErrSelectionIndexOutOfRange = errors.New("会话编号超出范围")
	// ErrTargetChanged 表示编号或选择指向的稳定 occupant 已变化。
	ErrTargetChanged = errors.New("Relay 目标已变化")
	// ErrNoSelection 表示用户尚未选择目标。
	ErrNoSelection = errors.New("尚未选择 Relay 目标")
)

// ClientKey 是一条 Relay 客户端连接的用户级唯一键。
type ClientKey struct {
	UserID    string
	MachineID string
}

// CatalogEntry 是目录中的稳定目标引用和当前展示信息。
type CatalogEntry struct {
	Ref     relayproto.SessionRef
	Session relayproto.Session
}

type machineState struct {
	connectionID string
	sequence     uint64
	sessions     []relayproto.Session
}

type routingState struct {
	numbered      []relayproto.SessionRef
	numberedValid bool
	selected      *relayproto.SessionRef
}

// SessionCatalog 保存所有在线机器的最新完整快照和用户路由状态。
//
// 目录只驻留内存；连接关闭会立即删除对应机器并使该用户编号快照失效。
type SessionCatalog struct {
	mu          sync.RWMutex
	connections map[string]ClientKey
	byKey       map[ClientKey]string
	machines    map[ClientKey]machineState
	routing     map[string]routingState
}

// NewSessionCatalog 创建空的在线会话目录。
func NewSessionCatalog() *SessionCatalog {
	return &SessionCatalog{
		connections: make(map[string]ClientKey),
		byKey:       make(map[ClientKey]string),
		machines:    make(map[ClientKey]machineState),
		routing:     make(map[string]routingState),
	}
}

// Attach 登记连接；重复 ClientKey 会拒绝新连接且保留旧连接。
func (catalog *SessionCatalog) Attach(connectionID string, key ClientKey) (bool, error) {
	if catalog == nil || strings.TrimSpace(connectionID) == "" {
		return false, ErrUnknownConnection
	}
	if err := relayproto.ValidateClientHello(relayproto.ClientHello{UserID: key.UserID, MachineID: key.MachineID}); err != nil {
		return false, err
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	if _, exists := catalog.connections[connectionID]; exists {
		return false, fmt.Errorf("%w: connection_id 重复", ErrDuplicateClient)
	}
	if _, exists := catalog.byKey[key]; exists {
		return false, ErrDuplicateClient
	}
	catalog.connections[connectionID] = key
	catalog.byKey[key] = connectionID
	catalog.machines[key] = machineState{connectionID: connectionID}
	return true, nil
}

// ApplySnapshot 原子应用当前连接的新完整快照。
func (catalog *SessionCatalog) ApplySnapshot(connectionID string, snapshot relayproto.SessionSnapshot) error {
	if catalog == nil {
		return ErrUnknownConnection
	}
	if err := relayproto.ValidateSnapshot(snapshot); err != nil {
		return err
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	key, exists := catalog.connections[connectionID]
	if !exists {
		return ErrUnknownConnection
	}
	current, exists := catalog.machines[key]
	if !exists || current.connectionID != connectionID {
		return ErrUnknownConnection
	}
	if snapshot.Sequence <= current.sequence {
		return ErrSnapshotStale
	}
	current.sequence = snapshot.Sequence
	current.sessions = append([]relayproto.Session(nil), snapshot.Sessions...)
	catalog.machines[key] = current
	catalog.invalidateChangedSelectionLocked(key.UserID)
	return nil
}

// Detach 删除连接和机器快照，并使该用户最近编号快照失效。
func (catalog *SessionCatalog) Detach(connectionID string) bool {
	if catalog == nil {
		return false
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	key, exists := catalog.connections[connectionID]
	if !exists {
		return false
	}
	delete(catalog.connections, connectionID)
	if catalog.byKey[key] == connectionID {
		delete(catalog.byKey, key)
	}
	if current, exists := catalog.machines[key]; exists && current.connectionID == connectionID {
		delete(catalog.machines, key)
	}
	routing := catalog.routing[key.UserID]
	routing.numbered = nil
	routing.numberedValid = false
	if routing.selected != nil && routing.selected.MachineID == key.MachineID {
		routing.selected = nil
	}
	catalog.routing[key.UserID] = routing
	return true
}

// HasMachine 报告用户机器当前是否在线。
func (catalog *SessionCatalog) HasMachine(userID, machineID string) bool {
	if catalog == nil {
		return false
	}
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	_, exists := catalog.machines[ClientKey{UserID: userID, MachineID: machineID}]
	return exists
}

// HasSessions 报告用户当前是否至少有一个在线 Agent 会话。
func (catalog *SessionCatalog) HasSessions(userID string) bool {
	if catalog == nil {
		return false
	}
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	for key, machine := range catalog.machines {
		if key.UserID == userID && machine.sequence > 0 && len(machine.sessions) > 0 {
			return true
		}
	}
	return false
}

// CreateNumberedSnapshot 聚合用户全部机器并替换最近编号快照。
func (catalog *SessionCatalog) CreateNumberedSnapshot(userID string) []CatalogEntry {
	if catalog == nil {
		return nil
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	entries := catalog.entriesLocked(userID)
	routing := catalog.routing[userID]
	routing.numbered = make([]relayproto.SessionRef, len(entries))
	for index := range entries {
		routing.numbered[index] = entries[index].Ref
	}
	routing.numberedValid = true
	catalog.routing[userID] = routing
	return cloneEntries(entries)
}

// ResolveNumbered 按最近一次列表中的一开始编号解析并复核当前稳定目标。
func (catalog *SessionCatalog) ResolveNumbered(userID string, index int) (CatalogEntry, error) {
	if catalog == nil {
		return CatalogEntry{}, ErrNoListSnapshot
	}
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	routing := catalog.routing[userID]
	if !routing.numberedValid {
		return CatalogEntry{}, ErrNoListSnapshot
	}
	if index < 1 || index > len(routing.numbered) {
		return CatalogEntry{}, fmt.Errorf("%w: 1 到 %d", ErrSelectionIndexOutOfRange, len(routing.numbered))
	}
	entry, ok := catalog.findEntryLocked(userID, routing.numbered[index-1])
	if !ok {
		return CatalogEntry{}, ErrTargetChanged
	}
	return entry, nil
}

// SetSelection 保存已经由客户端复核成功的当前选择。
func (catalog *SessionCatalog) SetSelection(userID string, target relayproto.SessionRef) error {
	if catalog == nil {
		return ErrTargetChanged
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	entry, ok := catalog.findEntryLocked(userID, target)
	if !ok {
		return ErrTargetChanged
	}
	routing := catalog.routing[userID]
	selected := entry.Ref
	routing.selected = &selected
	catalog.routing[userID] = routing
	return nil
}

// Selected 返回仍存在且 occupant 未变化的当前选择。
func (catalog *SessionCatalog) Selected(userID string) (CatalogEntry, error) {
	if catalog == nil {
		return CatalogEntry{}, ErrNoSelection
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	routing := catalog.routing[userID]
	if routing.selected == nil {
		return CatalogEntry{}, ErrNoSelection
	}
	entry, ok := catalog.findEntryLocked(userID, *routing.selected)
	if !ok {
		routing.selected = nil
		catalog.routing[userID] = routing
		return CatalogEntry{}, ErrNoSelection
	}
	return entry, nil
}

// ResolveTarget 从最新目录复核任意稳定目标，不修改用户编号或选择。
func (catalog *SessionCatalog) ResolveTarget(userID string, target relayproto.SessionRef) (CatalogEntry, error) {
	if catalog == nil {
		return CatalogEntry{}, ErrTargetChanged
	}
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	entry, ok := catalog.findEntryLocked(userID, target)
	if !ok {
		return CatalogEntry{}, ErrTargetChanged
	}
	return entry, nil
}

func (catalog *SessionCatalog) entriesLocked(userID string) []CatalogEntry {
	entries := make([]CatalogEntry, 0)
	for key, machine := range catalog.machines {
		if key.UserID != userID || machine.sequence == 0 {
			continue
		}
		for _, current := range machine.sessions {
			entries = append(entries, CatalogEntry{
				Ref: relayproto.SessionRef{
					MachineID: key.MachineID, LocalIndex: current.LocalIndex,
					PaneID: current.PaneID, OccupantHash: current.OccupantHash,
				},
				Session: current,
			})
		}
	}
	sort.Slice(entries, func(left, right int) bool {
		if entries[left].Ref.MachineID != entries[right].Ref.MachineID {
			return entries[left].Ref.MachineID < entries[right].Ref.MachineID
		}
		if entries[left].Ref.LocalIndex != entries[right].Ref.LocalIndex {
			return entries[left].Ref.LocalIndex < entries[right].Ref.LocalIndex
		}
		if entries[left].Ref.PaneID != entries[right].Ref.PaneID {
			return entries[left].Ref.PaneID < entries[right].Ref.PaneID
		}
		return entries[left].Ref.OccupantHash < entries[right].Ref.OccupantHash
	})
	return entries
}

func (catalog *SessionCatalog) findEntryLocked(userID string, target relayproto.SessionRef) (CatalogEntry, bool) {
	machine, exists := catalog.machines[ClientKey{UserID: userID, MachineID: target.MachineID}]
	if !exists || machine.sequence == 0 {
		return CatalogEntry{}, false
	}
	for _, current := range machine.sessions {
		if current.PaneID == target.PaneID && current.OccupantHash == target.OccupantHash {
			return CatalogEntry{
				Ref:     relayproto.SessionRef{MachineID: target.MachineID, LocalIndex: current.LocalIndex, PaneID: current.PaneID, OccupantHash: current.OccupantHash},
				Session: current,
			}, true
		}
	}
	return CatalogEntry{}, false
}

func (catalog *SessionCatalog) invalidateChangedSelectionLocked(userID string) {
	routing := catalog.routing[userID]
	if routing.selected == nil {
		return
	}
	if _, ok := catalog.findEntryLocked(userID, *routing.selected); !ok {
		routing.selected = nil
		catalog.routing[userID] = routing
	}
}

func cloneEntries(entries []CatalogEntry) []CatalogEntry {
	return append([]CatalogEntry(nil), entries...)
}
