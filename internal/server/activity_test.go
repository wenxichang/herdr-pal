package server

import (
	"testing"
	"time"
)

func TestUserActivityTrackerUsesSlidingWindow(t *testing.T) {
	tracker := newUserActivityTracker()
	start := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	if tracker.RecentlyActiveAndTouch("user-a", start, 2*time.Minute) {
		t.Fatal("first activity should not be recent")
	}
	if !tracker.RecentlyActiveAndTouch("user-a", start.Add(2*time.Minute), 2*time.Minute) {
		t.Fatal("activity at the exact boundary should be recent")
	}
	if !tracker.RecentlyActiveAndTouch("user-a", start.Add(3*time.Minute), 2*time.Minute) {
		t.Fatal("the boundary check should refresh the sliding window")
	}
	if tracker.RecentlyActiveAndTouch("user-a", start.Add(5*time.Minute+time.Nanosecond), 2*time.Minute) {
		t.Fatal("activity beyond the sliding window should not be recent")
	}
}

func TestUserActivityTrackerSeparatesUsers(t *testing.T) {
	tracker := newUserActivityTracker()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	tracker.Touch("user-a", now)

	if tracker.RecentlyActiveAndTouch("user-b", now.Add(time.Minute), 2*time.Minute) {
		t.Fatal("different users should not share activity")
	}
	if !tracker.RecentlyActiveAndTouch("user-a", now.Add(time.Minute), 2*time.Minute) {
		t.Fatal("same user should share activity across machines and sessions")
	}
}
