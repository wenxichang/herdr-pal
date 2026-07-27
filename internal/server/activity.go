package server

import (
	"sync"
	"time"
)

type userActivityTracker struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newUserActivityTracker() *userActivityTracker {
	return &userActivityTracker{last: make(map[string]time.Time)}
}

func (tracker *userActivityTracker) Touch(userID string, now time.Time) {
	if tracker == nil || userID == "" {
		return
	}
	tracker.mu.Lock()
	tracker.last[userID] = now
	tracker.mu.Unlock()
}

func (tracker *userActivityTracker) RecentlyActiveAndTouch(userID string, now time.Time, window time.Duration) bool {
	if tracker == nil || userID == "" {
		return false
	}
	tracker.mu.Lock()
	previous, exists := tracker.last[userID]
	tracker.last[userID] = now
	tracker.mu.Unlock()
	if !exists || window < 0 {
		return false
	}
	return previous.After(now) || now.Sub(previous) <= window
}
