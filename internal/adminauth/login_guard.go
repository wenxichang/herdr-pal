package adminauth

import (
	"crypto/sha256"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	loginFailureLimit = 5
	loginLockDuration = 15 * time.Minute
)

type loginBucket struct {
	failures    int
	lockedUntil time.Time
}

// LoginGuard 按规范化用户名和来源 IP 双桶限制连续登录失败。
type LoginGuard struct {
	mu sync.Mutex

	usernames map[[sha256.Size]byte]loginBucket
	sources   map[netip.Addr]loginBucket
	now       func() time.Time
}

// NewLoginGuard 创建第 5 次失败后锁定 15 分钟的登录保护器。
func NewLoginGuard(now func() time.Time) *LoginGuard {
	if now == nil {
		now = time.Now
	}
	return &LoginGuard{
		usernames: make(map[[sha256.Size]byte]loginBucket),
		sources:   make(map[netip.Addr]loginBucket),
		now:       now,
	}
}

// Allow 报告用户名和来源 IP 当前是否都未被锁定。
func (guard *LoginGuard) Allow(username string, source netip.Addr) bool {
	if guard == nil || !source.IsValid() {
		return false
	}
	userKey := loginUsernameKey(username)
	guard.mu.Lock()
	defer guard.mu.Unlock()
	now := guard.now().UTC()
	return bucketAllows(guard.usernames, userKey, now) && bucketAllows(guard.sources, source.Unmap(), now)
}

// Failure 记录用户名和来源 IP 的一次认证失败。
func (guard *LoginGuard) Failure(username string, source netip.Addr) {
	if guard == nil || !source.IsValid() {
		return
	}
	userKey := loginUsernameKey(username)
	source = source.Unmap()
	guard.mu.Lock()
	defer guard.mu.Unlock()
	now := guard.now().UTC()
	recordBucketFailure(guard.usernames, userKey, now)
	recordBucketFailure(guard.sources, source, now)
}

// Success 清除本次用户名和来源 IP 的全部失败记录。
func (guard *LoginGuard) Success(username string, source netip.Addr) {
	if guard == nil || !source.IsValid() {
		return
	}
	guard.mu.Lock()
	delete(guard.usernames, loginUsernameKey(username))
	delete(guard.sources, source.Unmap())
	guard.mu.Unlock()
}

func loginUsernameKey(username string) [sha256.Size]byte {
	username = strings.ToLower(strings.TrimSpace(strings.ToValidUTF8(username, "�")))
	return sha256.Sum256([]byte(username))
}

func bucketAllows[Key comparable](buckets map[Key]loginBucket, key Key, now time.Time) bool {
	bucket, exists := buckets[key]
	if !exists || bucket.lockedUntil.IsZero() {
		return true
	}
	if !now.Before(bucket.lockedUntil) {
		delete(buckets, key)
		return true
	}
	return false
}

func recordBucketFailure[Key comparable](buckets map[Key]loginBucket, key Key, now time.Time) {
	bucket := buckets[key]
	if !bucket.lockedUntil.IsZero() {
		if now.Before(bucket.lockedUntil) {
			return
		}
		bucket = loginBucket{}
	}
	bucket.failures++
	if bucket.failures >= loginFailureLimit {
		bucket.lockedUntil = now.Add(loginLockDuration)
	}
	buckets[key] = bucket
}
