package adminauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"sync"
	"time"
)

const (
	defaultSessionIdleTimeout     = 30 * time.Minute
	defaultSessionAbsoluteTimeout = 12 * time.Hour
	sessionRandomBytes            = 32
	maxSessionGenerateTries       = 32
)

var (
	// ErrInvalidSessionConfig 表示浏览器会话依赖或有效期配置无效。
	ErrInvalidSessionConfig = errors.New("管理员会话配置无效")
	// ErrSessionNotFound 对外统一表示会话不存在、已过期或格式无效。
	ErrSessionNotFound = errors.New("管理员会话不存在")
)

// SessionConfig 指定浏览器会话的有效期、时间和安全随机依赖。
type SessionConfig struct {
	IdleTimeout     time.Duration
	AbsoluteTimeout time.Duration
	Now             func() time.Time
	Random          io.Reader
}

// Session 是不包含 Cookie ID 和 CSRF 明文的管理员会话视图。
type Session struct {
	Username          string    `json:"username"`
	CreatedAt         time.Time `json:"created_at"`
	LastActiveAt      time.Time `json:"last_active_at"`
	AbsoluteExpiresAt time.Time `json:"absolute_expires_at"`
}

// SessionCredentials 是仅返回给浏览器一次的会话 ID、CSRF Token 和安全视图。
type SessionCredentials struct {
	ID        string  `json:"-"`
	CSRFToken string  `json:"csrf_token"`
	Session   Session `json:"session"`
}

type sessionRecord struct {
	Session
	csrfDigest [sha256.Size]byte
}

// SessionManager 以内存摘要保存浏览器会话，并实施空闲与绝对有效期。
type SessionManager struct {
	mu sync.Mutex

	sessions        map[[sha256.Size]byte]sessionRecord
	idleTimeout     time.Duration
	absoluteTimeout time.Duration
	now             func() time.Time
	random          io.Reader
}

// NewSessionManager 创建默认空闲 30 分钟、绝对有效期 12 小时的会话管理器。
func NewSessionManager(config SessionConfig) (*SessionManager, error) {
	if config.IdleTimeout == 0 {
		config.IdleTimeout = defaultSessionIdleTimeout
	}
	if config.AbsoluteTimeout == 0 {
		config.AbsoluteTimeout = defaultSessionAbsoluteTimeout
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.IdleTimeout < 0 || config.AbsoluteTimeout < 0 || config.IdleTimeout > config.AbsoluteTimeout {
		return nil, ErrInvalidSessionConfig
	}
	return &SessionManager{
		sessions: make(map[[sha256.Size]byte]sessionRecord), idleTimeout: config.IdleTimeout,
		absoluteTimeout: config.AbsoluteTimeout, now: config.Now, random: config.Random,
	}, nil
}

// Create 为规范化管理员创建一组新的浏览器会话凭据。
func (manager *SessionManager) Create(username string) (SessionCredentials, error) {
	if manager == nil {
		return SessionCredentials{}, ErrInvalidSessionConfig
	}
	username, err := normalizeUsername(username)
	if err != nil {
		return SessionCredentials{}, err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	now := manager.now().UTC()
	manager.purgeExpiredLocked(now)
	id, idDigest, csrf, csrfDigest, err := manager.generateCredentialsLocked()
	if err != nil {
		return SessionCredentials{}, err
	}
	record := sessionRecord{Session: Session{
		Username: username, CreatedAt: now, LastActiveAt: now,
		AbsoluteExpiresAt: now.Add(manager.absoluteTimeout),
	}, csrfDigest: csrfDigest}
	manager.sessions[idDigest] = record
	return SessionCredentials{ID: id, CSRFToken: csrf, Session: record.Session}, nil
}

// Get 返回有效会话并更新其最后活动时间。
func (manager *SessionManager) Get(id string) (Session, bool) {
	if manager == nil {
		return Session{}, false
	}
	idDigest, ok := sessionDigest(id)
	if !ok {
		return Session{}, false
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	record, exists := manager.sessions[idDigest]
	if !exists {
		return Session{}, false
	}
	now := manager.now().UTC()
	if manager.expired(record, now) {
		delete(manager.sessions, idDigest)
		return Session{}, false
	}
	record.LastActiveAt = now
	manager.sessions[idDigest] = record
	return record.Session, true
}

// VerifyCSRF 使用常量时间比较验证指定会话的 CSRF Token。
func (manager *SessionManager) VerifyCSRF(id, token string) bool {
	if manager == nil {
		return false
	}
	idDigest, ok := sessionDigest(id)
	if !ok {
		return false
	}
	csrfDigest, ok := sessionDigest(token)
	if !ok {
		return false
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	record, exists := manager.sessions[idDigest]
	if !exists {
		return false
	}
	if manager.expired(record, manager.now().UTC()) {
		delete(manager.sessions, idDigest)
		return false
	}
	return subtle.ConstantTimeCompare(record.csrfDigest[:], csrfDigest[:]) == 1
}

// Rotate 替换会话 ID 和 CSRF Token，同时保留原始绝对到期时间。
func (manager *SessionManager) Rotate(id string) (SessionCredentials, error) {
	if manager == nil {
		return SessionCredentials{}, ErrSessionNotFound
	}
	idDigest, ok := sessionDigest(id)
	if !ok {
		return SessionCredentials{}, ErrSessionNotFound
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	record, exists := manager.sessions[idDigest]
	if !exists {
		return SessionCredentials{}, ErrSessionNotFound
	}
	now := manager.now().UTC()
	if manager.expired(record, now) {
		delete(manager.sessions, idDigest)
		return SessionCredentials{}, ErrSessionNotFound
	}
	newID, newIDDigest, csrf, csrfDigest, err := manager.generateCredentialsLocked()
	if err != nil {
		return SessionCredentials{}, err
	}
	record.LastActiveAt = now
	record.csrfDigest = csrfDigest
	delete(manager.sessions, idDigest)
	manager.sessions[newIDDigest] = record
	return SessionCredentials{ID: newID, CSRFToken: csrf, Session: record.Session}, nil
}

// Delete 撤销指定会话；格式无效或不存在时保持幂等。
func (manager *SessionManager) Delete(id string) {
	if manager == nil {
		return
	}
	digest, ok := sessionDigest(id)
	if !ok {
		return
	}
	manager.mu.Lock()
	delete(manager.sessions, digest)
	manager.mu.Unlock()
}

// RevokeUser 撤销指定管理员的全部浏览器会话。
func (manager *SessionManager) RevokeUser(username string) {
	manager.RevokeUserExcept(username, "")
}

// RevokeUserExcept 撤销指定管理员除 keepID 外的全部浏览器会话。
func (manager *SessionManager) RevokeUserExcept(username, keepID string) {
	if manager == nil {
		return
	}
	username, err := normalizeUsername(username)
	if err != nil {
		return
	}
	keepDigest, keepValid := sessionDigest(keepID)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for digest, record := range manager.sessions {
		if record.Username == username && (!keepValid || digest != keepDigest) {
			delete(manager.sessions, digest)
		}
	}
}

func (manager *SessionManager) generateCredentialsLocked() (string, [sha256.Size]byte, string, [sha256.Size]byte, error) {
	for attempt := 0; attempt < maxSessionGenerateTries; attempt++ {
		id, err := generateSessionToken(manager.random)
		if err != nil {
			return "", [sha256.Size]byte{}, "", [sha256.Size]byte{}, err
		}
		idDigest := sha256.Sum256([]byte(id))
		if _, exists := manager.sessions[idDigest]; exists {
			continue
		}
		csrf, err := generateSessionToken(manager.random)
		if err != nil {
			return "", [sha256.Size]byte{}, "", [sha256.Size]byte{}, err
		}
		return id, idDigest, csrf, sha256.Sum256([]byte(csrf)), nil
	}
	return "", [sha256.Size]byte{}, "", [sha256.Size]byte{}, ErrInvalidSessionConfig
}

func (manager *SessionManager) purgeExpiredLocked(now time.Time) {
	for digest, record := range manager.sessions {
		if manager.expired(record, now) {
			delete(manager.sessions, digest)
		}
	}
}

func (manager *SessionManager) expired(record sessionRecord, now time.Time) bool {
	return !now.Before(record.AbsoluteExpiresAt) || now.Sub(record.LastActiveAt) >= manager.idleTimeout
}

func generateSessionToken(random io.Reader) (string, error) {
	value := make([]byte, sessionRandomBytes)
	if _, err := io.ReadFull(random, value); err != nil {
		return "", ErrInvalidSessionConfig
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func sessionDigest(value string) ([sha256.Size]byte, bool) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(decoded) != sessionRandomBytes || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return [sha256.Size]byte{}, false
	}
	return sha256.Sum256([]byte(value)), true
}
