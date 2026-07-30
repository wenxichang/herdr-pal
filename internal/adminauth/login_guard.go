package adminauth

import (
	"crypto/sha256"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	loginFailureLimit   = 5
	loginLockDuration   = 15 * time.Minute
	loginSweepInterval  = time.Minute
	loginBucketCapacity = 4096
)

type loginBucket struct {
	failures    int
	inFlight    bool
	lockedUntil time.Time
	lastSeen    time.Time
}

// LoginGuard 按规范化用户名和来源 IP 双桶限制登录失败及并发密码校验。
type LoginGuard struct {
	mu sync.Mutex

	usernames map[[sha256.Size]byte]loginBucket
	sources   map[netip.Addr]loginBucket
	now       func() time.Time
	lastSweep time.Time
}

// NewLoginGuard 创建第 5 次失败后锁定 15 分钟且状态容量有界的登录保护器。
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

// Begin 原子保留一次密码校验；同一用户名或来源同时只允许一个在途请求。
func (guard *LoginGuard) Begin(username string, source netip.Addr) bool {
	if guard == nil || !source.IsValid() {
		return false
	}
	userKey := loginUsernameKey(username)
	source = source.Unmap()
	guard.mu.Lock()
	defer guard.mu.Unlock()

	now := guard.now().UTC()
	guard.sweepIfDue(now)
	userBucket, userExists := activeLoginBucket(guard.usernames, userKey, now)
	sourceBucket, sourceExists := activeLoginBucket(guard.sources, source, now)
	if !loginBucketAllows(userBucket, userExists, now) || !loginBucketAllows(sourceBucket, sourceExists, now) {
		return false
	}
	if (!userExists && len(guard.usernames) >= loginBucketCapacity) || (!sourceExists && len(guard.sources) >= loginBucketCapacity) {
		return false
	}
	userBucket.inFlight = true
	userBucket.lastSeen = now
	sourceBucket.inFlight = true
	sourceBucket.lastSeen = now
	guard.usernames[userKey] = userBucket
	guard.sources[source] = sourceBucket
	return true
}

// Finish 完成已保留的密码校验，并报告失败后用户名或来源是否已锁定。
func (guard *LoginGuard) Finish(username string, source netip.Addr, succeeded bool) bool {
	if guard == nil || !source.IsValid() {
		return true
	}
	userKey := loginUsernameKey(username)
	source = source.Unmap()
	guard.mu.Lock()
	defer guard.mu.Unlock()

	if succeeded {
		delete(guard.usernames, userKey)
		delete(guard.sources, source)
		return false
	}
	now := guard.now().UTC()
	userLocked := finishLoginFailure(guard.usernames, userKey, now)
	sourceLocked := finishLoginFailure(guard.sources, source, now)
	return userLocked || sourceLocked
}

func (guard *LoginGuard) sweepIfDue(now time.Time) {
	if !guard.lastSweep.IsZero() && now.Sub(guard.lastSweep) < loginSweepInterval {
		return
	}
	guard.lastSweep = now
	sweepLoginBuckets(guard.usernames, now)
	sweepLoginBuckets(guard.sources, now)
}

func loginUsernameKey(username string) [sha256.Size]byte {
	username = strings.ToLower(strings.TrimSpace(strings.ToValidUTF8(username, "�")))
	return sha256.Sum256([]byte(username))
}

func activeLoginBucket[Key comparable](buckets map[Key]loginBucket, key Key, now time.Time) (loginBucket, bool) {
	bucket, exists := buckets[key]
	if exists && loginBucketExpired(bucket, now) {
		delete(buckets, key)
		return loginBucket{}, false
	}
	return bucket, exists
}

func loginBucketAllows(bucket loginBucket, exists bool, now time.Time) bool {
	if !exists {
		return true
	}
	if bucket.inFlight {
		return false
	}
	return bucket.lockedUntil.IsZero() || !now.Before(bucket.lockedUntil)
}

func finishLoginFailure[Key comparable](buckets map[Key]loginBucket, key Key, now time.Time) bool {
	bucket, exists := buckets[key]
	if !exists {
		return true
	}
	bucket.inFlight = false
	bucket.failures++
	bucket.lastSeen = now
	if bucket.failures >= loginFailureLimit {
		bucket.lockedUntil = now.Add(loginLockDuration)
	}
	buckets[key] = bucket
	return !bucket.lockedUntil.IsZero() && now.Before(bucket.lockedUntil)
}

func sweepLoginBuckets[Key comparable](buckets map[Key]loginBucket, now time.Time) {
	for key, bucket := range buckets {
		if loginBucketExpired(bucket, now) {
			delete(buckets, key)
		}
	}
}

func loginBucketExpired(bucket loginBucket, now time.Time) bool {
	if !bucket.lockedUntil.IsZero() {
		return !now.Before(bucket.lockedUntil)
	}
	return !bucket.lastSeen.IsZero() && !now.Before(bucket.lastSeen.Add(loginLockDuration))
}
