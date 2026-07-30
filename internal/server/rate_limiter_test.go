package server

import (
	"testing"
	"time"
)

func TestUserRateLimiterRejectsSecondInputWithinOneSecond(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := NewUserRateLimiter(1, 20, func() time.Time { return now })
	if decision := limiter.Allow("user-1"); !decision.Allowed {
		t.Fatalf("first decision = %#v", decision)
	}
	decision := limiter.Allow("user-1")
	if decision.Allowed || decision.Window != "second" || decision.RetryAfter != time.Second {
		t.Fatalf("second decision = %#v", decision)
	}
	now = now.Add(time.Second)
	if decision := limiter.Allow("user-1"); !decision.Allowed {
		t.Fatalf("after window decision = %#v", decision)
	}
}

func TestUserRateLimiterRejectsTwentyFirstInputWithinMinute(t *testing.T) {
	now := time.Unix(200, 0)
	limiter := NewUserRateLimiter(0, 20, func() time.Time { return now })
	for index := 0; index < 20; index++ {
		if decision := limiter.Allow("user-1"); !decision.Allowed {
			t.Fatalf("decision %d = %#v", index, decision)
		}
		now = now.Add(time.Second)
	}
	decision := limiter.Allow("user-1")
	if decision.Allowed || decision.Window != "minute" || decision.RetryAfter != 40*time.Second {
		t.Fatalf("twenty-first decision = %#v", decision)
	}
}

func TestUserRateLimiterUsesLongestRetryAndRejectedInputDoesNotExtendWindow(t *testing.T) {
	now := time.Unix(300, 0)
	limiter := NewUserRateLimiter(1, 1, func() time.Time { return now })
	if decision := limiter.Allow("user-1"); !decision.Allowed {
		t.Fatalf("first decision = %#v", decision)
	}
	now = now.Add(500 * time.Millisecond)
	decision := limiter.Allow("user-1")
	if decision.Allowed || decision.Window != "second,minute" || decision.RetryAfter != 60*time.Second-500*time.Millisecond {
		t.Fatalf("combined decision = %#v", decision)
	}
	now = now.Add(60*time.Second - 500*time.Millisecond)
	if decision := limiter.Allow("user-1"); !decision.Allowed {
		t.Fatalf("decision after original window = %#v", decision)
	}
}

func TestUserRateLimiterSupportsDisabledWindowsAndIndependentUsers(t *testing.T) {
	now := time.Unix(400, 0)
	disabled := NewUserRateLimiter(0, 0, func() time.Time { return now })
	for index := 0; index < 100; index++ {
		if decision := disabled.Allow("user-1"); !decision.Allowed {
			t.Fatalf("disabled decision %d = %#v", index, decision)
		}
	}
	limited := NewUserRateLimiter(1, 0, func() time.Time { return now })
	if !limited.Allow("user-1").Allowed || !limited.Allow("user-2").Allowed {
		t.Fatal("different users should have independent windows")
	}
}

func TestUserRateLimiterHandlesClockRollbackWithoutNegativeRetry(t *testing.T) {
	now := time.Unix(500, 0)
	limiter := NewUserRateLimiter(1, 0, func() time.Time { return now })
	if decision := limiter.Allow("user-1"); !decision.Allowed {
		t.Fatalf("first decision = %#v", decision)
	}
	now = now.Add(-10 * time.Second)
	decision := limiter.Allow("user-1")
	if decision.Allowed || decision.RetryAfter != 11*time.Second {
		t.Fatalf("rollback decision = %#v", decision)
	}
}

func TestUserRateLimiterLazilyRemovesExpiredUsers(t *testing.T) {
	now := time.Unix(600, 0)
	limiter := NewUserRateLimiter(1, 20, func() time.Time { return now })
	limiter.Allow("old-user")
	now = now.Add(61 * time.Second)
	limiter.Allow("new-user")
	limiter.mu.Lock()
	_, oldExists := limiter.users["old-user"]
	limiter.mu.Unlock()
	if oldExists {
		t.Fatal("expired user state was not removed")
	}
}
