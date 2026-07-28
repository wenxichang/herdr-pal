package credential

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"
)

const storeVersion = 2

var (
	// ErrInsecurePermissions 表示凭据文件可被当前用户之外的主体读取或写入。
	ErrInsecurePermissions = errors.New("HPRP 凭据文件权限不安全")
	// ErrCredentialConflict 表示凭据文件中存在重复 credential ID。
	ErrCredentialConflict = errors.New("HPRP credential ID 冲突")
	// ErrCredentialNotFound 表示管理操作指定的 credential ID 不存在。
	ErrCredentialNotFound = errors.New("HPRP credential 不存在")
	// ErrCredentialIDExhausted 表示当前存储已经无法分配更大的 credential ID。
	ErrCredentialIDExhausted = errors.New("HPRP credential ID 已耗尽")
)

type storeFile struct {
	Version     int      `json:"version"`
	Credentials []Record `json:"credentials"`
}

// Store 是支持并发认证、动态管理和原子持久化的文件凭据存储。
type Store struct {
	path string

	mu      sync.RWMutex
	records map[uint64]Record
	nextID  uint64
	now     func() time.Time
	random  io.Reader
}

// LoadStore 从权限受限的 JSON 文件加载凭据；文件不存在时返回从 ID 1 开始的空存储。
func LoadStore(path string) (*Store, error) {
	if filepath.Clean(path) == "." || path == "" {
		return nil, ErrInvalidRecord
	}
	store := &Store{path: path, records: make(map[uint64]Record), nextID: 1, now: time.Now, random: rand.Reader}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取凭据文件状态: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, ErrInvalidRecord
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, ErrInsecurePermissions
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取凭据文件: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var persisted storeFile
	if err := decoder.Decode(&persisted); err != nil {
		return nil, fmt.Errorf("%w: 解码凭据文件", ErrInvalidRecord)
	}
	if err := requireStoreEOF(decoder); err != nil || persisted.Version != storeVersion {
		return nil, ErrInvalidRecord
	}
	var maximumID uint64
	for _, record := range persisted.Credentials {
		if err := validateRecord(record); err != nil {
			return nil, err
		}
		if _, exists := store.records[record.CredentialID]; exists {
			return nil, ErrCredentialConflict
		}
		store.records[record.CredentialID] = cloneRecord(record)
		if record.CredentialID > maximumID {
			maximumID = record.CredentialID
		}
	}
	if maximumID == math.MaxUint64 {
		store.nextID = 0
	} else {
		store.nextID = maximumID + 1
	}
	return store, nil
}

// VerifyBearer 验证 Key、生命周期和真实来源地址并返回绑定身份。
func (store *Store) VerifyBearer(_ context.Context, token string, source netip.Addr) (Identity, error) {
	if store == nil {
		return Identity{}, ErrUnauthenticated
	}
	credentialID, err := BearerCredentialID(token)
	if err != nil {
		return Identity{}, ErrUnauthenticated
	}
	store.mu.RLock()
	record, exists := store.records[credentialID]
	now := store.now()
	store.mu.RUnlock()
	if !exists {
		return Identity{}, ErrUnauthenticated
	}
	identity, err := VerifyRecord(record, token, now, source)
	if err != nil {
		if errors.Is(err, ErrInvalidRecord) {
			return Identity{}, err
		}
		return Identity{}, ErrUnauthenticated
	}
	return identity, nil
}

// Issue 生成新 Key、原子持久化摘要记录并只向调用方返回一次完整 Key。
func (store *Store) Issue(principalID, machineID string, allowedSources []string, expiresAt *time.Time) (string, Record, error) {
	if store == nil {
		return "", Record{}, ErrInvalidRecord
	}
	rules, err := NormalizeSourceRules(allowedSources)
	if err != nil {
		return "", Record{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.nextID == 0 {
		return "", Record{}, ErrCredentialIDExhausted
	}
	credentialID := store.nextID
	token, record, err := Issue(credentialID, principalID, machineID, rules, expiresAt, store.now(), store.random)
	if err != nil {
		return "", Record{}, err
	}
	candidate := cloneRecordMap(store.records)
	candidate[credentialID] = record
	if err := store.persistRecordsLocked(candidate); err != nil {
		return "", Record{}, err
	}
	store.records = candidate
	if credentialID == math.MaxUint64 {
		store.nextID = 0
	} else {
		store.nextID++
	}
	return token, cloneRecord(record), nil
}

// List 返回按 credential ID 升序排列的凭据记录快照。
func (store *Store) List() []Record {
	if store == nil {
		return nil
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	return sortedRecords(store.records)
}

// Show 返回指定凭据的独立记录快照。
func (store *Store) Show(credentialID uint64) (Record, error) {
	if store == nil || credentialID == 0 {
		return Record{}, ErrCredentialNotFound
	}
	store.mu.RLock()
	record, exists := store.records[credentialID]
	store.mu.RUnlock()
	if !exists {
		return Record{}, ErrCredentialNotFound
	}
	return cloneRecord(record), nil
}

// Enable 持久化启用指定凭据。
func (store *Store) Enable(credentialID uint64) (Record, error) {
	return store.updateRecord(credentialID, func(record Record) (Record, error) {
		if record.Status == StatusEnabled {
			return record, nil
		}
		record.Status = StatusEnabled
		record.UpdatedAt = store.now().UTC()
		return record, nil
	})
}

// Disable 持久化禁用指定凭据；连接撤下由调用方在成功后执行。
func (store *Store) Disable(credentialID uint64) (Record, error) {
	return store.updateRecord(credentialID, func(record Record) (Record, error) {
		if record.Status == StatusDisabled {
			return record, nil
		}
		record.Status = StatusDisabled
		record.UpdatedAt = store.now().UTC()
		return record, nil
	})
}

// Delete 原子删除指定凭据并返回被删除的记录快照。
func (store *Store) Delete(credentialID uint64) (Record, error) {
	if store == nil || credentialID == 0 {
		return Record{}, ErrCredentialNotFound
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	record, exists := store.records[credentialID]
	if !exists {
		return Record{}, ErrCredentialNotFound
	}
	candidate := cloneRecordMap(store.records)
	delete(candidate, credentialID)
	if err := store.persistRecordsLocked(candidate); err != nil {
		return Record{}, err
	}
	store.records = candidate
	return cloneRecord(record), nil
}

// AddSources 规范化并添加来源规则，重复规则保持幂等。
func (store *Store) AddSources(credentialID uint64, values []string) (Record, error) {
	return store.updateRecord(credentialID, func(record Record) (Record, error) {
		combined := make([]string, 0, len(record.AllowedSources)+len(values))
		for _, rule := range record.AllowedSources {
			combined = append(combined, string(rule))
		}
		combined = append(combined, values...)
		rules, err := NormalizeSourceRules(combined)
		if err != nil {
			return Record{}, err
		}
		record.AllowedSources = rules
		record.UpdatedAt = store.now().UTC()
		return record, nil
	})
}

// RemoveSources 按规范化后的完整规则删除来源，且不允许产生空策略。
func (store *Store) RemoveSources(credentialID uint64, values []string) (Record, error) {
	removeRules, err := NormalizeSourceRules(values)
	if err != nil {
		return Record{}, err
	}
	remove := make(map[SourceRule]struct{}, len(removeRules))
	for _, rule := range removeRules {
		remove[rule] = struct{}{}
	}
	return store.updateRecord(credentialID, func(record Record) (Record, error) {
		remaining := make([]SourceRule, 0, len(record.AllowedSources))
		for _, rule := range record.AllowedSources {
			if _, exists := remove[rule]; !exists {
				remaining = append(remaining, rule)
			}
		}
		if len(remaining) == 0 {
			return Record{}, ErrSourceRequired
		}
		record.AllowedSources = remaining
		record.UpdatedAt = store.now().UTC()
		return record, nil
	})
}

// SetSources 使用新的非空规范化规则集合替换来源策略。
func (store *Store) SetSources(credentialID uint64, values []string) (Record, error) {
	rules, err := NormalizeSourceRules(values)
	if err != nil {
		return Record{}, err
	}
	return store.updateRecord(credentialID, func(record Record) (Record, error) {
		record.AllowedSources = rules
		record.UpdatedAt = store.now().UTC()
		return record, nil
	})
}

func (store *Store) updateRecord(credentialID uint64, transform func(Record) (Record, error)) (Record, error) {
	if store == nil || credentialID == 0 || transform == nil {
		return Record{}, ErrCredentialNotFound
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	current, exists := store.records[credentialID]
	if !exists {
		return Record{}, ErrCredentialNotFound
	}
	updated, err := transform(cloneRecord(current))
	if err != nil {
		return Record{}, err
	}
	if err := validateRecord(updated); err != nil {
		return Record{}, err
	}
	if reflectRecordsEqual(current, updated) {
		return cloneRecord(current), nil
	}
	candidate := cloneRecordMap(store.records)
	candidate[credentialID] = cloneRecord(updated)
	if err := store.persistRecordsLocked(candidate); err != nil {
		return Record{}, err
	}
	store.records = candidate
	return cloneRecord(updated), nil
}

func (store *Store) persistRecordsLocked(records map[uint64]Record) error {
	directory := filepath.Dir(store.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("创建凭据目录: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(directory, 0o700); err != nil {
			return fmt.Errorf("收紧凭据目录权限: %w", err)
		}
	}
	encoded, err := json.MarshalIndent(storeFile{Version: storeVersion, Credentials: sortedRecords(records)}, "", "  ")
	if err != nil {
		return fmt.Errorf("编码凭据文件: %w", err)
	}
	encoded = append(encoded, '\n')
	temporary, err := os.CreateTemp(directory, ".credentials-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时凭据文件: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("设置临时凭据权限: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("写入临时凭据文件: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("同步临时凭据文件: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭临时凭据文件: %w", err)
	}
	if err := os.Rename(temporaryPath, store.path); err != nil {
		return fmt.Errorf("替换凭据文件: %w", err)
	}
	if runtime.GOOS != "windows" {
		directoryHandle, err := os.Open(directory)
		if err != nil {
			return fmt.Errorf("打开凭据目录: %w", err)
		}
		syncErr := directoryHandle.Sync()
		closeErr := directoryHandle.Close()
		if syncErr != nil {
			return fmt.Errorf("同步凭据目录: %w", syncErr)
		}
		if closeErr != nil {
			return fmt.Errorf("关闭凭据目录: %w", closeErr)
		}
	}
	return nil
}

func sortedRecords(records map[uint64]Record) []Record {
	result := make([]Record, 0, len(records))
	for _, record := range records {
		result = append(result, cloneRecord(record))
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].CredentialID < result[right].CredentialID
	})
	return result
}

func cloneRecordMap(records map[uint64]Record) map[uint64]Record {
	result := make(map[uint64]Record, len(records))
	for credentialID, record := range records {
		result[credentialID] = cloneRecord(record)
	}
	return result
}

func cloneRecord(record Record) Record {
	record.AllowedSources = append([]SourceRule(nil), record.AllowedSources...)
	if record.ExpiresAt != nil {
		expiresAt := *record.ExpiresAt
		record.ExpiresAt = &expiresAt
	}
	return record
}

func reflectRecordsEqual(left, right Record) bool {
	if left.CredentialID != right.CredentialID || left.PrincipalID != right.PrincipalID || left.MachineID != right.MachineID ||
		left.SecretSHA256 != right.SecretSHA256 || left.Status != right.Status || !left.CreatedAt.Equal(right.CreatedAt) ||
		!left.UpdatedAt.Equal(right.UpdatedAt) || len(left.AllowedSources) != len(right.AllowedSources) {
		return false
	}
	if (left.ExpiresAt == nil) != (right.ExpiresAt == nil) || left.ExpiresAt != nil && !left.ExpiresAt.Equal(*right.ExpiresAt) {
		return false
	}
	for index := range left.AllowedSources {
		if left.AllowedSources[index] != right.AllowedSources[index] {
			return false
		}
	}
	return true
}

func requireStoreEOF(decoder *json.Decoder) error {
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ErrInvalidRecord
	}
	return nil
}
