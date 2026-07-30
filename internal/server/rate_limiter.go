package server

import (
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	rateLimitSecondWindow  = time.Second
	rateLimitMinuteWindow  = time.Minute
	rateLimitSweepInterval = time.Minute
)

// RateLimitDecision 描述一次用户输入是否被接受及需要等待的窗口。
type RateLimitDecision struct {
	Allowed    bool
	RetryAfter time.Duration
	Window     string
}

// UserRateLimiter 按企业微信用户维护两个滚动时间窗口。
type UserRateLimiter struct {
	mu          sync.Mutex
	perSecond   int
	perMinute   int
	now         func() time.Time
	users       map[string][]time.Time
	lastSweepAt time.Time
}

// NewUserRateLimiter 创建一个按用户隔离的滚动窗口限速器，0 表示禁用对应窗口。
func NewUserRateLimiter(perSecond, perMinute int, now func() time.Time) *UserRateLimiter {
	if now == nil {
		now = time.Now
	}
	return &UserRateLimiter{
		perSecond: perSecond,
		perMinute: perMinute,
		now:       now,
		users:     make(map[string][]time.Time),
	}
}

// Limits 返回当前启用的秒级和分钟级输入上限。
func (limiter *UserRateLimiter) Limits() (int, int) {
	if limiter == nil {
		return 0, 0
	}
	return limiter.perSecond, limiter.perMinute
}

// Allow 判断并记录一条唯一用户输入；被拒绝的输入不会延长窗口。
func (limiter *UserRateLimiter) Allow(userID string) RateLimitDecision {
	if limiter == nil || (limiter.perSecond <= 0 && limiter.perMinute <= 0) {
		return RateLimitDecision{Allowed: true}
	}
	now := limiter.now()
	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	limiter.sweepExpired(now)
	timestamps := limiter.prune(limiter.users[userID], now)
	limiter.users[userID] = timestamps

	windows := make([]string, 0, 2)
	retryAfter := time.Duration(0)
	if wait, exceeded := windowRetry(timestamps, now, rateLimitSecondWindow, limiter.perSecond); exceeded {
		windows = append(windows, "second")
		if wait > retryAfter {
			retryAfter = wait
		}
	}
	if wait, exceeded := windowRetry(timestamps, now, rateLimitMinuteWindow, limiter.perMinute); exceeded {
		windows = append(windows, "minute")
		if wait > retryAfter {
			retryAfter = wait
		}
	}
	if len(windows) > 0 {
		if retryAfter <= 0 {
			retryAfter = time.Nanosecond
		}
		return RateLimitDecision{RetryAfter: retryAfter, Window: strings.Join(windows, ",")}
	}
	limiter.users[userID] = append(timestamps, now)
	return RateLimitDecision{Allowed: true}
}

func (limiter *UserRateLimiter) prune(timestamps []time.Time, now time.Time) []time.Time {
	window := rateLimitMinuteWindow
	if limiter.perMinute <= 0 {
		window = rateLimitSecondWindow
	}
	kept := timestamps[:0]
	for _, timestamp := range timestamps {
		age := now.Sub(timestamp)
		if age < 0 || age < window {
			kept = append(kept, timestamp)
		}
	}
	return kept
}

func (limiter *UserRateLimiter) sweepExpired(now time.Time) {
	if !limiter.lastSweepAt.IsZero() && now.Sub(limiter.lastSweepAt) < rateLimitSweepInterval {
		return
	}
	for userID, timestamps := range limiter.users {
		kept := limiter.prune(timestamps, now)
		if len(kept) == 0 {
			delete(limiter.users, userID)
		} else {
			limiter.users[userID] = kept
		}
	}
	limiter.lastSweepAt = now
}

func windowRetry(timestamps []time.Time, now time.Time, window time.Duration, limit int) (time.Duration, bool) {
	if limit <= 0 {
		return 0, false
	}
	eligible := make([]time.Time, 0, len(timestamps))
	for _, timestamp := range timestamps {
		age := now.Sub(timestamp)
		if age < 0 || age < window {
			eligible = append(eligible, timestamp)
		}
	}
	if len(eligible) < limit {
		return 0, false
	}
	sort.Slice(eligible, func(left, right int) bool { return eligible[left].Before(eligible[right]) })
	expiresAt := eligible[len(eligible)-limit].Add(window)
	retryAfter := expiresAt.Sub(now)
	if retryAfter <= 0 {
		retryAfter = time.Nanosecond
	}
	return retryAfter, true
}
