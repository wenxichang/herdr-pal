package notificationprefs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/hprp"
)

// ReconcileResult 是同一目录视图的核对统计和开关状态副本。
type ReconcileResult struct {
	Added             int
	Removed           int
	RemovedIdentities []Identity
	States            map[Identity]bool
}

type preferenceRecord struct {
	Identity Identity `json:"identity"`
	Enabled  *bool    `json:"enabled"`
}

type preferenceFile struct {
	Version int                `json:"version"`
	Records []preferenceRecord `json:"records"`
}

// Store 并发安全地保存开关，先原子替换文件再发布整批内存状态。
type Store struct {
	mu          sync.RWMutex
	path        string
	records     map[Identity]bool
	replaceFile func(string, string) error
	syncFile    func(*os.File) error
}

// NewMemoryStore 创建互相隔离、不写磁盘的偏好存储。
func NewMemoryStore() *Store {
	return &Store{records: make(map[Identity]bool), replaceFile: os.Rename, syncFile: func(file *os.File) error { return file.Sync() }}
}

// LoadStore 加载通知偏好；文件不存在时返回空存储，损坏或权限不安全时拒绝加载。
func LoadStore(path string) (*Store, error) {
	if path == "" || filepath.Clean(path) == "." {
		return nil, ErrInvalidPreference
	}
	store := NewMemoryStore()
	store.path = path
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("检查通知偏好文件: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: 文件类型或权限不安全", ErrInvalidPreference)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取通知偏好文件: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var persisted preferenceFile
	if decoder.Decode(&persisted) != nil || persisted.Version != 1 || persisted.Records == nil {
		return nil, ErrInvalidPreference
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, ErrInvalidPreference
	}
	for _, record := range persisted.Records {
		if record.Enabled == nil || !validIdentity(record.Identity) {
			return nil, ErrInvalidPreference
		}
		if _, exists := store.records[record.Identity]; exists {
			return nil, ErrInvalidPreference
		}
		store.records[record.Identity] = *record.Enabled
	}
	return store, nil
}

// Lookup 返回显式状态及存在标志；不存在时状态默认为开启。
func (store *Store) Lookup(key Identity) (bool, bool) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	enabled, exists := store.records[key]
	if !exists {
		return true, false
	}
	return enabled, true
}

// Enabled 返回在线业务组合的有效开关，无记录时默认为开启。
func (store *Store) Enabled(key Identity) bool { enabled, _ := store.Lookup(key); return enabled }

// SetBatch 一次更新去重后的有效组合；持久化失败不发布任何内存变更。
func (store *Store) SetBatch(keys []Identity, enabled bool) error {
	for _, key := range keys {
		if !validIdentity(key) {
			return ErrInvalidPreference
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	candidate := cloneRecords(store.records)
	for _, key := range keys {
		candidate[key] = enabled
	}
	return store.commitLocked(candidate)
}

// Reconcile 核对指定用户的已同步在线机器，为新组合添加默认开启记录。
// 仅清理本次列表非空的机器；离线、未同步或空列表的机器均保留已有设置。
func (store *Store) Reconcile(userID string, machineIDs []string, current []Identity) (ReconcileResult, error) {
	if credential.ValidatePrincipalID(userID) != nil {
		return ReconcileResult{}, ErrInvalidPreference
	}
	machines := make(map[string]bool, len(machineIDs))
	for _, machine := range machineIDs {
		if hprp.ValidateMachineID(machine) != nil {
			return ReconcileResult{}, ErrInvalidPreference
		}
		machines[machine] = true
	}
	active := make(map[Identity]bool, len(current))
	nonemptyMachines := make(map[string]bool)
	for _, key := range current {
		if !validIdentity(key) || key.PrincipalID != userID || !machines[key.MachineID] {
			return ReconcileResult{}, ErrInvalidPreference
		}
		active[key] = true
		nonemptyMachines[key.MachineID] = true
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	candidate := cloneRecords(store.records)
	result := ReconcileResult{States: make(map[Identity]bool, len(active))}
	for key := range candidate {
		if key.PrincipalID == userID && machines[key.MachineID] && nonemptyMachines[key.MachineID] && !active[key] {
			delete(candidate, key)
			result.Removed++
			result.RemovedIdentities = append(result.RemovedIdentities, key)
		}
	}
	for key := range active {
		if _, exists := candidate[key]; !exists {
			candidate[key] = true
			result.Added++
		}
		result.States[key] = candidate[key]
	}
	if err := store.commitLocked(candidate); err != nil {
		return ReconcileResult{}, err
	}
	return result, nil
}

func cloneRecords(records map[Identity]bool) map[Identity]bool {
	result := make(map[Identity]bool, len(records))
	for key, value := range records {
		result[key] = value
	}
	return result
}

func (store *Store) commitLocked(candidate map[Identity]bool) error {
	if maps.Equal(store.records, candidate) {
		return nil
	}
	if store.path != "" {
		if err := store.persistLocked(candidate); err != nil {
			return err
		}
	}
	store.records = candidate
	return nil
}

func (store *Store) persistLocked(records map[Identity]bool) error {
	persisted := preferenceFile{Version: 1, Records: make([]preferenceRecord, 0, len(records))}
	for key, enabled := range records {
		value := enabled
		persisted.Records = append(persisted.Records, preferenceRecord{Identity: key, Enabled: &value})
	}
	sort.Slice(persisted.Records, func(i, j int) bool {
		left, right := persisted.Records[i].Identity, persisted.Records[j].Identity
		a, b := []string{left.PrincipalID, left.MachineID, left.Title, left.Workspace, left.Tab, left.CLIType}, []string{right.PrincipalID, right.MachineID, right.Title, right.Workspace, right.Tab, right.CLIType}
		for n := range a {
			if a[n] != b[n] {
				return a[n] < b[n]
			}
		}
		return false
	})
	data, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return err
	}
	directory := filepath.Dir(store.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".notification-preferences-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := store.syncFile(file); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := store.replaceFile(file.Name(), store.path); err != nil {
		return err
	}
	// 替换完成即已提交；目录同步失败不能让内存停留在旧状态。
	if dir, err := os.Open(directory); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
