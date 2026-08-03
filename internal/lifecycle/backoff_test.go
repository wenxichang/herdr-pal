package lifecycle

import (
	"testing"
	"time"
)

func TestBackoffUsesBoundedExponentialSequence(t *testing.T) {
	backoff, err := NewBackoff(time.Second, 30*time.Second)
	if err != nil {
		t.Fatalf("NewBackoff() error = %v", err)
	}
	want := []time.Duration{
		time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
		30 * time.Second,
	}
	for index, expected := range want {
		if got := backoff.Next(); got != expected {
			t.Fatalf("Next() call %d = %s, want %s", index+1, got, expected)
		}
	}
}

func TestBackoffResetRestoresMinimum(t *testing.T) {
	backoff, err := NewBackoff(time.Second, 30*time.Second)
	if err != nil {
		t.Fatalf("NewBackoff() error = %v", err)
	}
	_ = backoff.Next()
	_ = backoff.Next()
	backoff.Reset()
	if got := backoff.Next(); got != time.Second {
		t.Fatalf("Next() after Reset() = %s", got)
	}
}

func TestNewBackoffRejectsInvalidBounds(t *testing.T) {
	for _, bounds := range [][2]time.Duration{
		{0, time.Second},
		{time.Second, 0},
		{2 * time.Second, time.Second},
	} {
		if _, err := NewBackoff(bounds[0], bounds[1]); err == nil {
			t.Fatalf("NewBackoff(%s, %s) should fail", bounds[0], bounds[1])
		}
	}
}
