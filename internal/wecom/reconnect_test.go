package wecom

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBackoffGrowsCapsJittersAndResets(t *testing.T) {
	backoff := NewBackoff(time.Second, 30*time.Second, func() float64 { return 0.5 })
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for index, expected := range want {
		if got := backoff.Next(); got != expected {
			t.Fatalf("Next() attempt %d = %s, want %s", index+1, got, expected)
		}
	}
	backoff.Reset()
	if got := backoff.Next(); got != time.Second {
		t.Fatalf("Next() after Reset = %s, want 1s", got)
	}
}

func TestBackoffClampsRandomValue(t *testing.T) {
	for _, random := range []float64{-4, 2} {
		backoff := NewBackoff(time.Second, 30*time.Second, func() float64 { return random })
		got := backoff.Next()
		if got < 800*time.Millisecond || got > 1200*time.Millisecond {
			t.Fatalf("Next() random=%v = %s, want jitter in [800ms, 1.2s]", random, got)
		}
	}
}

func TestBackoffNeverExceedsConfiguredMaximumAfterJitter(t *testing.T) {
	backoff := NewBackoff(time.Second, 30*time.Second, func() float64 { return 1 })
	for index := 0; index < 8; index++ {
		if got := backoff.Next(); got > 30*time.Second {
			t.Fatalf("Next() attempt %d = %s, exceeds configured maximum", index+1, got)
		}
	}
}

func TestWaitBackoffStopsWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := WaitBackoff(ctx, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitBackoff() error = %v, want context.Canceled", err)
	}
}
