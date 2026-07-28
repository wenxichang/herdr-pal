package credential

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

const storeVersion = 1

var (
	// ErrInsecurePermissions 表示凭据文件可被当前用户之外的主体读取或写入。
	ErrInsecurePermissions = errors.New("HPRP 凭据文件权限不安全")
	// ErrCredentialConflict 表示签发得到的 credential ID 已经存在。
	ErrCredentialConflict = errors.New("HPRP credential ID 冲突")
)

type storeFile struct {
	Version     int      `json:"version"`
	Credentials []Record `json:"credentials"`
}

// Store 是支持并发验证和原子签发的文件凭据存储。
type Store struct {
	path string

	mu      sync.RWMutex
	records map[string]Record
	now     func() time.Time
	random  io.Reader
}

// LoadStore 从权限受限的 JSON 文件加载凭据；文件不存在时返回空存储。
func LoadStore(path string) (*Store, error) {
	if filepath.Clean(path) == "." || path == "" {
		return nil, ErrInvalidRecord
	}
	store := &Store{path: path, records: make(map[string]Record), now: time.Now, random: rand.Reader}
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
	for _, record := range persisted.Credentials {
		if err := validateRecord(record); err != nil {
			return nil, err
		}
		if _, exists := store.records[record.CredentialID]; exists {
			return nil, ErrCredentialConflict
		}
		store.records[record.CredentialID] = record
	}
	return store, nil
}

// VerifyBearer 验证 Key 并返回其绑定的用户和机器身份。
func (store *Store) VerifyBearer(_ context.Context, token string) (Identity, error) {
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
	identity, err := VerifyRecord(record, token, now)
	if err != nil {
		if errors.Is(err, ErrInvalidRecord) {
			return Identity{}, err
		}
		return Identity{}, ErrUnauthenticated
	}
	return identity, nil
}

// Issue 生成新 Key、原子持久化摘要记录并只向调用方返回一次完整 Key。
func (store *Store) Issue(principalID, machineID string) (string, Record, error) {
	if store == nil {
		return "", Record{}, ErrInvalidRecord
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	token, record, err := Issue(principalID, machineID, store.now(), store.random)
	if err != nil {
		return "", Record{}, err
	}
	if _, exists := store.records[record.CredentialID]; exists {
		return "", Record{}, ErrCredentialConflict
	}
	store.records[record.CredentialID] = record
	if err := store.saveLocked(); err != nil {
		delete(store.records, record.CredentialID)
		return "", Record{}, err
	}
	return token, record, nil
}

func (store *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(store.path), 0o700); err != nil {
		return fmt.Errorf("创建凭据目录: %w", err)
	}
	records := make([]Record, 0, len(store.records))
	for _, record := range store.records {
		records = append(records, record)
	}
	encoded, err := json.MarshalIndent(storeFile{Version: storeVersion, Credentials: records}, "", "  ")
	if err != nil {
		return fmt.Errorf("编码凭据文件: %w", err)
	}
	encoded = append(encoded, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(store.path), ".credentials-*.tmp")
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
	return nil
}

func requireStoreEOF(decoder *json.Decoder) error {
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ErrInvalidRecord
	}
	return nil
}
