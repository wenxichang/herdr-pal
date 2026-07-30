package adminauth

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
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	authFileVersion       = 1
	initialPasswordBytes  = 24
	maxTokenGenerateTries = 32
)

var (
	// ErrInvalidAuthFile 表示管理员认证文件损坏、版本未知或记录不满足安全约束。
	ErrInvalidAuthFile = errors.New("管理员认证文件无效")
	// ErrInsecureAuthPermissions 表示认证文件可被当前用户之外的主体读取或写入。
	ErrInsecureAuthPermissions = errors.New("管理员认证文件权限不安全")
	// ErrInvalidUsername 表示管理员用户名不符合固定格式。
	ErrInvalidUsername = errors.New("管理员用户名无效")
	// ErrAdminExists 表示规范化后的管理员用户名已经存在。
	ErrAdminExists = errors.New("管理员已存在")
	// ErrAdminNotFound 表示指定管理员不存在。
	ErrAdminNotFound = errors.New("管理员不存在")
	// ErrAuthenticationFailed 对外统一表示管理员密码或自动化 Token 认证失败。
	ErrAuthenticationFailed = errors.New("管理员认证失败")
	// ErrLastAdmin 表示操作会删除认证文件中的最后一个管理员。
	ErrLastAdmin = errors.New("不能删除最后一个管理员")
	// ErrAutomationTokenConflict 表示安全随机源连续生成了重复 Token ID。
	ErrAutomationTokenConflict = errors.New("管理员自动化 Token ID 冲突")
)

var usernamePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{2,31}$`)

// Options 指定管理员认证存储的时间和安全随机依赖。
type Options struct {
	Now    func() time.Time
	Random io.Reader
}

// Bootstrap 只在首次创建默认管理员时返回一次明文引导凭据。
type Bootstrap struct {
	Created         bool
	Username        string
	InitialPassword string
	AutomationToken string
}

// AutomationToken 是不包含 Secret 摘要和明文 Token 的安全视图。
type AutomationToken struct {
	TokenID   string    `json:"token_id"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Admin 是不包含密码摘要和 Token Secret 的管理员视图。
type Admin struct {
	Username           string          `json:"username"`
	MustChangePassword bool            `json:"must_change_password"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	AutomationToken    AutomationToken `json:"automation_token"`
}

// CreatedAdmin 只在创建成功时返回一次初始密码和自动化 Token。
type CreatedAdmin struct {
	Admin           Admin  `json:"admin"`
	InitialPassword string `json:"initial_password"`
	AutomationToken string `json:"automation_token"`
}

// AutomationIdentity 是自动化 Bearer Token 认证成功后的管理员身份。
type AutomationIdentity struct {
	Username string
	TokenID  string
}

type adminRecord struct {
	Username           string                `json:"username"`
	PasswordHash       string                `json:"password_hash"`
	MustChangePassword bool                  `json:"must_change_password"`
	CreatedAt          time.Time             `json:"created_at"`
	UpdatedAt          time.Time             `json:"updated_at"`
	AutomationToken    AutomationTokenRecord `json:"automation_token"`
}

type authFile struct {
	Version int           `json:"version"`
	Admins  []adminRecord `json:"admins"`
}

// Store 原子持久化多个管理员的密码摘要和自动化 Token 摘要。
type Store struct {
	path string

	mu      sync.RWMutex
	admins  map[string]adminRecord
	now     func() time.Time
	random  io.Reader
	codec   Argon2idCodec
	syncDir func(string) error
}

// Load 加载认证文件；文件不存在时创建默认 admin 并只返回一次引导明文。
func Load(path string, options Options) (*Store, Bootstrap, error) {
	if strings.TrimSpace(path) == "" || filepath.Clean(path) == "." {
		return nil, Bootstrap{}, ErrInvalidAuthFile
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Random == nil {
		options.Random = rand.Reader
	}
	store := &Store{
		path: path, admins: make(map[string]adminRecord), now: options.Now,
		random: options.Random, codec: NewArgon2idCodec(), syncDir: syncAuthDirectory,
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		bootstrap, err := store.bootstrap()
		if err != nil {
			return nil, Bootstrap{}, err
		}
		return store, bootstrap, nil
	}
	if err != nil {
		return nil, Bootstrap{}, fmt.Errorf("读取管理员认证文件状态: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, Bootstrap{}, ErrInvalidAuthFile
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, Bootstrap{}, ErrInsecureAuthPermissions
	}
	if runtime.GOOS != "windows" {
		directoryInfo, err := os.Stat(filepath.Dir(path))
		if err != nil {
			return nil, Bootstrap{}, fmt.Errorf("读取管理员认证目录状态: %w", err)
		}
		if !directoryInfo.IsDir() || directoryInfo.Mode().Perm()&0o077 != 0 {
			return nil, Bootstrap{}, ErrInsecureAuthPermissions
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, Bootstrap{}, fmt.Errorf("读取管理员认证文件: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var persisted authFile
	if err := decoder.Decode(&persisted); err != nil {
		return nil, Bootstrap{}, fmt.Errorf("%w: 解码 JSON", ErrInvalidAuthFile)
	}
	if err := requireAuthEOF(decoder); err != nil || persisted.Version != authFileVersion || len(persisted.Admins) == 0 {
		return nil, Bootstrap{}, ErrInvalidAuthFile
	}
	tokenIDs := make(map[string]struct{}, len(persisted.Admins))
	for _, record := range persisted.Admins {
		if err := validateAdminRecord(record); err != nil {
			return nil, Bootstrap{}, err
		}
		if _, exists := store.admins[record.Username]; exists {
			return nil, Bootstrap{}, ErrInvalidAuthFile
		}
		if _, exists := tokenIDs[record.AutomationToken.TokenID]; exists {
			return nil, Bootstrap{}, ErrInvalidAuthFile
		}
		tokenIDs[record.AutomationToken.TokenID] = struct{}{}
		store.admins[record.Username] = record
	}
	return store, Bootstrap{}, nil
}

// Authenticate 验证用户名和密码并返回安全管理员视图。
func (store *Store) Authenticate(username, password string) (Admin, error) {
	if store == nil {
		return Admin{}, ErrAuthenticationFailed
	}
	username, err := normalizeUsername(username)
	if err != nil {
		return Admin{}, ErrAuthenticationFailed
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	record, exists := store.admins[username]
	if !exists {
		return Admin{}, ErrAuthenticationFailed
	}
	ok, err := store.codec.Verify(record.PasswordHash, password)
	if err != nil || !ok {
		return Admin{}, ErrAuthenticationFailed
	}
	return adminView(record), nil
}

// Admin 返回指定管理员的安全视图。
func (store *Store) Admin(username string) (Admin, error) {
	if store == nil {
		return Admin{}, ErrAdminNotFound
	}
	username, err := normalizeUsername(username)
	if err != nil {
		return Admin{}, ErrAdminNotFound
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	record, exists := store.admins[username]
	if !exists {
		return Admin{}, ErrAdminNotFound
	}
	return adminView(record), nil
}

// ListAdmins 返回按用户名升序排列的安全管理员快照。
func (store *Store) ListAdmins() []Admin {
	if store == nil {
		return nil
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	result := make([]Admin, 0, len(store.admins))
	for _, record := range store.admins {
		result = append(result, adminView(record))
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Username < result[right].Username })
	return result
}

// CreateAdmin 创建管理员，并只返回一次随机初始密码和自动化 Token。
func (store *Store) CreateAdmin(username string) (CreatedAdmin, error) {
	if store == nil {
		return CreatedAdmin{}, ErrInvalidAuthFile
	}
	username, err := normalizeUsername(username)
	if err != nil {
		return CreatedAdmin{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.admins[username]; exists {
		return CreatedAdmin{}, ErrAdminExists
	}
	record, initialPassword, token, err := store.newAdminRecordLocked(username)
	if err != nil {
		return CreatedAdmin{}, err
	}
	candidate := cloneAdminRecords(store.admins)
	candidate[username] = record
	if err := store.persistLocked(candidate); err != nil {
		return CreatedAdmin{}, err
	}
	store.admins = candidate
	return CreatedAdmin{Admin: adminView(record), InitialPassword: initialPassword, AutomationToken: token}, nil
}

// ChangePassword 验证当前密码后设置新密码，并清除首次改密标记。
func (store *Store) ChangePassword(username, currentPassword, newPassword string) error {
	if store == nil {
		return ErrAdminNotFound
	}
	username, err := normalizeUsername(username)
	if err != nil {
		return ErrAdminNotFound
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	record, exists := store.admins[username]
	if !exists {
		return ErrAdminNotFound
	}
	ok, verifyErr := store.codec.Verify(record.PasswordHash, currentPassword)
	if verifyErr != nil || !ok {
		return ErrAuthenticationFailed
	}
	hash, err := store.codec.Hash(newPassword, store.random)
	if err != nil {
		return err
	}
	record.PasswordHash = hash
	record.MustChangePassword = false
	record.UpdatedAt = store.now().UTC()
	return store.replaceRecordLocked(record)
}

// ResetPassword 设置随机初始密码并返回一次明文，不轮换自动化 Token。
func (store *Store) ResetPassword(username string) (string, error) {
	if store == nil {
		return "", ErrAdminNotFound
	}
	username, err := normalizeUsername(username)
	if err != nil {
		return "", ErrAdminNotFound
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	record, exists := store.admins[username]
	if !exists {
		return "", ErrAdminNotFound
	}
	password, err := generateInitialPassword(store.random)
	if err != nil {
		return "", err
	}
	hash, err := store.codec.Hash(password, store.random)
	if err != nil {
		return "", err
	}
	record.PasswordHash = hash
	record.MustChangePassword = true
	record.UpdatedAt = store.now().UTC()
	if err := store.replaceRecordLocked(record); err != nil {
		return "", err
	}
	return password, nil
}

// DeleteAdmin 删除指定管理员，同时使其自动化 Token 立即失效。
func (store *Store) DeleteAdmin(requester, target string) error {
	if store == nil {
		return ErrAdminNotFound
	}
	requester, requesterErr := normalizeUsername(requester)
	target, targetErr := normalizeUsername(target)
	if requesterErr != nil || targetErr != nil {
		return ErrAdminNotFound
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.admins[requester]; !exists {
		return ErrAdminNotFound
	}
	if _, exists := store.admins[target]; !exists {
		return ErrAdminNotFound
	}
	if len(store.admins) == 1 {
		return ErrLastAdmin
	}
	candidate := cloneAdminRecords(store.admins)
	delete(candidate, target)
	if err := store.persistLocked(candidate); err != nil {
		return err
	}
	store.admins = candidate
	return nil
}

// RotateAutomationToken 替换指定管理员的 Token，并只返回一次新明文。
func (store *Store) RotateAutomationToken(username string) (string, AutomationToken, error) {
	if store == nil {
		return "", AutomationToken{}, ErrAdminNotFound
	}
	username, err := normalizeUsername(username)
	if err != nil {
		return "", AutomationToken{}, ErrAdminNotFound
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	record, exists := store.admins[username]
	if !exists {
		return "", AutomationToken{}, ErrAdminNotFound
	}
	token, tokenRecord, err := store.generateUniqueTokenLocked()
	if err != nil {
		return "", AutomationToken{}, err
	}
	record.AutomationToken = tokenRecord
	record.UpdatedAt = store.now().UTC()
	if err := store.replaceRecordLocked(record); err != nil {
		return "", AutomationToken{}, err
	}
	return token, automationTokenView(tokenRecord), nil
}

// SetAutomationTokenEnabled 启用或禁用指定管理员的自动化 Token。
func (store *Store) SetAutomationTokenEnabled(username string, enabled bool) (AutomationToken, error) {
	if store == nil {
		return AutomationToken{}, ErrAdminNotFound
	}
	username, err := normalizeUsername(username)
	if err != nil {
		return AutomationToken{}, ErrAdminNotFound
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	record, exists := store.admins[username]
	if !exists {
		return AutomationToken{}, ErrAdminNotFound
	}
	if record.AutomationToken.Enabled == enabled {
		return automationTokenView(record.AutomationToken), nil
	}
	now := store.now().UTC()
	record.AutomationToken.Enabled = enabled
	record.AutomationToken.UpdatedAt = now
	record.UpdatedAt = now
	if err := store.replaceRecordLocked(record); err != nil {
		return AutomationToken{}, err
	}
	return automationTokenView(record.AutomationToken), nil
}

// VerifyAutomationBearer 验证明文自动化 Token 并返回绑定管理员身份。
func (store *Store) VerifyAutomationBearer(value string) (AutomationIdentity, error) {
	if store == nil {
		return AutomationIdentity{}, ErrAuthenticationFailed
	}
	tokenID, _, ok := parseAutomationToken(value)
	if !ok {
		return AutomationIdentity{}, ErrAuthenticationFailed
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	for username, record := range store.admins {
		if record.AutomationToken.TokenID != tokenID {
			continue
		}
		if !record.AutomationToken.Enabled || !VerifyAutomationToken(record.AutomationToken, value) {
			return AutomationIdentity{}, ErrAuthenticationFailed
		}
		return AutomationIdentity{Username: username, TokenID: tokenID}, nil
	}
	return AutomationIdentity{}, ErrAuthenticationFailed
}

func (store *Store) bootstrap() (Bootstrap, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	record, password, token, err := store.newAdminRecordLocked("admin")
	if err != nil {
		return Bootstrap{}, err
	}
	candidate := map[string]adminRecord{"admin": record}
	if err := store.persistLocked(candidate); err != nil {
		return Bootstrap{}, err
	}
	store.admins = candidate
	return Bootstrap{Created: true, Username: "admin", InitialPassword: password, AutomationToken: token}, nil
}

func (store *Store) newAdminRecordLocked(username string) (adminRecord, string, string, error) {
	password, err := generateInitialPassword(store.random)
	if err != nil {
		return adminRecord{}, "", "", err
	}
	hash, err := store.codec.Hash(password, store.random)
	if err != nil {
		return adminRecord{}, "", "", err
	}
	token, tokenRecord, err := store.generateUniqueTokenLocked()
	if err != nil {
		return adminRecord{}, "", "", err
	}
	now := store.now().UTC()
	return adminRecord{
		Username: username, PasswordHash: hash, MustChangePassword: true,
		CreatedAt: now, UpdatedAt: now, AutomationToken: tokenRecord,
	}, password, token, nil
}

func (store *Store) generateUniqueTokenLocked() (string, AutomationTokenRecord, error) {
	for attempt := 0; attempt < maxTokenGenerateTries; attempt++ {
		token, record, err := GenerateAutomationToken(store.random, store.now())
		if err != nil {
			return "", AutomationTokenRecord{}, err
		}
		conflict := false
		for _, current := range store.admins {
			if current.AutomationToken.TokenID == record.TokenID {
				conflict = true
				break
			}
		}
		if !conflict {
			return token, record, nil
		}
	}
	return "", AutomationTokenRecord{}, ErrAutomationTokenConflict
}

func (store *Store) replaceRecordLocked(record adminRecord) error {
	candidate := cloneAdminRecords(store.admins)
	candidate[record.Username] = record
	if err := store.persistLocked(candidate); err != nil {
		return err
	}
	store.admins = candidate
	return nil
}

func (store *Store) persistLocked(admins map[string]adminRecord) error {
	directory := filepath.Dir(store.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("创建管理员认证目录: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(directory, 0o700); err != nil {
			return fmt.Errorf("收紧管理员认证目录权限: %w", err)
		}
	}
	encoded, err := json.MarshalIndent(authFile{Version: authFileVersion, Admins: sortedAdminRecords(admins)}, "", "  ")
	if err != nil {
		return fmt.Errorf("编码管理员认证文件: %w", err)
	}
	encoded = append(encoded, '\n')
	temporary, err := os.CreateTemp(directory, ".server-auth-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时管理员认证文件: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("设置临时管理员认证文件权限: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("写入临时管理员认证文件: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("同步临时管理员认证文件: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭临时管理员认证文件: %w", err)
	}
	if err := os.Rename(temporaryPath, store.path); err != nil {
		return fmt.Errorf("替换管理员认证文件: %w", err)
	}
	if runtime.GOOS != "windows" && store.syncDir != nil {
		_ = store.syncDir(directory)
	}
	return nil
}

func normalizeUsername(username string) (string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if !usernamePattern.MatchString(username) {
		return "", ErrInvalidUsername
	}
	return username, nil
}

func validateAdminRecord(record adminRecord) error {
	normalized, err := normalizeUsername(record.Username)
	if err != nil || normalized != record.Username || record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() || record.UpdatedAt.Before(record.CreatedAt) {
		return ErrInvalidAuthFile
	}
	if _, _, _, err := parsePasswordHash(record.PasswordHash); err != nil {
		return ErrInvalidAuthFile
	}
	if !validAutomationTokenRecord(record.AutomationToken) {
		return ErrInvalidAuthFile
	}
	return nil
}

func generateInitialPassword(random io.Reader) (string, error) {
	if random == nil {
		return "", ErrInvalidPassword
	}
	value := make([]byte, initialPasswordBytes)
	if _, err := io.ReadFull(random, value); err != nil {
		return "", fmt.Errorf("%w: 读取初始密码随机数", ErrInvalidPassword)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func adminView(record adminRecord) Admin {
	return Admin{
		Username: record.Username, MustChangePassword: record.MustChangePassword,
		CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC(),
		AutomationToken: automationTokenView(record.AutomationToken),
	}
}

func automationTokenView(record AutomationTokenRecord) AutomationToken {
	return AutomationToken{
		TokenID: record.TokenID, Enabled: record.Enabled,
		CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC(),
	}
}

func cloneAdminRecords(records map[string]adminRecord) map[string]adminRecord {
	result := make(map[string]adminRecord, len(records))
	for username, record := range records {
		result[username] = record
	}
	return result
}

func sortedAdminRecords(records map[string]adminRecord) []adminRecord {
	result := make([]adminRecord, 0, len(records))
	for _, record := range records {
		result = append(result, record)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Username < result[right].Username })
	return result
}

func requireAuthEOF(decoder *json.Decoder) error {
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ErrInvalidAuthFile
	}
	return nil
}

func syncAuthDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	syncErr := handle.Sync()
	closeErr := handle.Close()
	return errors.Join(syncErr, closeErr)
}
