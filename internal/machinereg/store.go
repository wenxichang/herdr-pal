package machinereg

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/hprp"
)

const storeVersion = 1
const registrationIDRandomBytes = 16
const registrationIDMaxAttempts = 8

type storeFile struct {
	Version       int       `json:"version"`
	Registrations []Request `json:"registrations"`
}

// StoreOptions 提供注册存储的时钟与安全随机源，主要用于确定性测试。
type StoreOptions struct {
	Now    func() time.Time
	Random io.Reader
}

// Store 是并发安全、原子持久化的待审批注册申请存储。
type Store struct {
	path string

	mu            sync.RWMutex
	requests      map[string]Request
	now           func() time.Time
	random        io.Reader
	syncDirectory func(string) error
}

// LoadStore 加载待审批申请；文件不存在时返回空存储。
func LoadStore(path string, options StoreOptions) (*Store, error) {
	if path == "" || filepath.Clean(path) == "." {
		return nil, ErrInvalidRequest
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Random == nil {
		options.Random = rand.Reader
	}
	store := &Store{
		path: path, requests: make(map[string]Request), now: options.Now, random: options.Random,
		syncDirectory: syncStoreDirectory,
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取机器注册申请文件状态: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, ErrInvalidRequest
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, ErrInsecurePermissions
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取机器注册申请文件: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var persisted storeFile
	if err := decoder.Decode(&persisted); err != nil {
		return nil, fmt.Errorf("%w: 解码机器注册申请文件", ErrInvalidRequest)
	}
	if err := requireStoreEOF(decoder); err != nil || persisted.Version != storeVersion {
		return nil, ErrInvalidRequest
	}
	identities := make(map[string]struct{}, len(persisted.Registrations))
	for _, request := range persisted.Registrations {
		if err := validateRequest(request); err != nil {
			return nil, err
		}
		if _, exists := store.requests[request.RegistrationID]; exists {
			return nil, ErrInvalidRequest
		}
		identity := request.PrincipalID + "\x00" + request.MachineID
		if _, exists := identities[identity]; exists {
			return nil, ErrInvalidRequest
		}
		identities[identity] = struct{}{}
		store.requests[request.RegistrationID] = cloneRequest(request)
	}
	return store, nil
}

// Create 创建待审批申请；同一用户和机器已有申请时返回原申请且 created 为 false。
func (store *Store) Create(principalID, machineID string, sources []credential.SourceRule) (Request, bool, error) {
	if store == nil || credential.ValidatePrincipalID(principalID) != nil || hprp.ValidateMachineID(machineID) != nil || validateSources(sources) != nil {
		return Request{}, false, ErrInvalidRequest
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, request := range store.requests {
		if request.PrincipalID == principalID && request.MachineID == machineID {
			return cloneRequest(request), false, nil
		}
	}
	registrationID := ""
	for range registrationIDMaxAttempts {
		candidateID, err := newRegistrationID(store.random)
		if err != nil {
			return Request{}, false, fmt.Errorf("%w: 生成注册申请 ID", ErrInvalidRequest)
		}
		if _, exists := store.requests[candidateID]; !exists {
			registrationID = candidateID
			break
		}
	}
	if registrationID == "" {
		return Request{}, false, fmt.Errorf("%w: 注册申请 ID 冲突", ErrInvalidRequest)
	}
	request := Request{
		RegistrationID: registrationID,
		PrincipalID:    principalID,
		MachineID:      machineID,
		AllowedSources: append([]credential.SourceRule(nil), sources...),
		RequestedAt:    store.now().UTC(),
	}
	if err := validateRequest(request); err != nil {
		return Request{}, false, err
	}
	candidate := cloneRequestMap(store.requests)
	candidate[registrationID] = request
	if err := store.persistLocked(candidate); err != nil {
		return Request{}, false, err
	}
	store.requests = candidate
	return cloneRequest(request), true, nil
}

// List 返回按申请时间和申请 ID 稳定排序的待审批申请快照。
func (store *Store) List() []Request {
	if store == nil {
		return nil
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	return sortedRequests(store.requests)
}

// Show 返回指定待审批申请。
func (store *Store) Show(registrationID string) (Request, error) {
	if store == nil {
		return Request{}, ErrRequestNotFound
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	request, exists := store.requests[registrationID]
	if !exists {
		return Request{}, ErrRequestNotFound
	}
	return cloneRequest(request), nil
}

// Find 按用户和机器查找待审批申请。
func (store *Store) Find(principalID, machineID string) (Request, bool) {
	if store == nil {
		return Request{}, false
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	for _, request := range store.requests {
		if request.PrincipalID == principalID && request.MachineID == machineID {
			return cloneRequest(request), true
		}
	}
	return Request{}, false
}

// HasPrincipal 报告指定用户是否存在任意待审批申请。
func (store *Store) HasPrincipal(principalID string) bool {
	if store == nil {
		return false
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	for _, request := range store.requests {
		if request.PrincipalID == principalID {
			return true
		}
	}
	return false
}

// Delete 原子删除指定待审批申请并返回被删除的快照。
func (store *Store) Delete(registrationID string) (Request, error) {
	if store == nil {
		return Request{}, ErrRequestNotFound
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	request, exists := store.requests[registrationID]
	if !exists {
		return Request{}, ErrRequestNotFound
	}
	candidate := cloneRequestMap(store.requests)
	delete(candidate, registrationID)
	if err := store.persistLocked(candidate); err != nil {
		return Request{}, err
	}
	store.requests = candidate
	return cloneRequest(request), nil
}

func (store *Store) persistLocked(requests map[string]Request) error {
	directory := filepath.Dir(store.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("创建机器注册申请目录: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(directory, 0o700); err != nil {
			return fmt.Errorf("收紧机器注册申请目录权限: %w", err)
		}
	}
	encoded, err := json.MarshalIndent(storeFile{Version: storeVersion, Registrations: sortedRequests(requests)}, "", "  ")
	if err != nil {
		return fmt.Errorf("编码机器注册申请文件: %w", err)
	}
	encoded = append(encoded, '\n')
	temporary, err := os.CreateTemp(directory, ".registrations-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时机器注册申请文件: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("设置临时机器注册申请文件权限: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("写入临时机器注册申请文件: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("同步临时机器注册申请文件: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭临时机器注册申请文件: %w", err)
	}
	if err := os.Rename(temporaryPath, store.path); err != nil {
		return fmt.Errorf("替换机器注册申请文件: %w", err)
	}
	if runtime.GOOS != "windows" && store.syncDirectory != nil {
		_ = store.syncDirectory(directory)
	}
	return nil
}

func validateRequest(request Request) error {
	if !validRegistrationID(request.RegistrationID) || credential.ValidatePrincipalID(request.PrincipalID) != nil ||
		hprp.ValidateMachineID(request.MachineID) != nil || request.RequestedAt.IsZero() || validateSources(request.AllowedSources) != nil {
		return ErrInvalidRequest
	}
	return nil
}

func validateSources(sources []credential.SourceRule) error {
	values := make([]string, len(sources))
	for index, source := range sources {
		values[index] = string(source)
	}
	normalized, err := credential.NormalizeSourceRules(values)
	if err != nil || len(normalized) != len(sources) {
		return ErrInvalidRequest
	}
	for index := range sources {
		if normalized[index] != sources[index] {
			return ErrInvalidRequest
		}
	}
	return nil
}

func newRegistrationID(random io.Reader) (string, error) {
	value := make([]byte, registrationIDRandomBytes)
	if _, err := io.ReadFull(random, value); err != nil {
		return "", err
	}
	return "reg_" + base64.RawURLEncoding.EncodeToString(value), nil
}

func validRegistrationID(value string) bool {
	if !strings.HasPrefix(value, "reg_") {
		return false
	}
	encoded := strings.TrimPrefix(value, "reg_")
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	return err == nil && len(decoded) == registrationIDRandomBytes && base64.RawURLEncoding.EncodeToString(decoded) == encoded
}

func sortedRequests(requests map[string]Request) []Request {
	result := make([]Request, 0, len(requests))
	for _, request := range requests {
		result = append(result, cloneRequest(request))
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].RequestedAt.Equal(result[right].RequestedAt) {
			return result[left].RegistrationID < result[right].RegistrationID
		}
		return result[left].RequestedAt.Before(result[right].RequestedAt)
	})
	return result
}

func cloneRequestMap(requests map[string]Request) map[string]Request {
	result := make(map[string]Request, len(requests))
	for registrationID, request := range requests {
		result[registrationID] = cloneRequest(request)
	}
	return result
}

func cloneRequest(request Request) Request {
	request.AllowedSources = append([]credential.SourceRule(nil), request.AllowedSources...)
	return request
}

func requireStoreEOF(decoder *json.Decoder) error {
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ErrInvalidRequest
	}
	return nil
}

func syncStoreDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	return errors.Join(handle.Sync(), handle.Close())
}
