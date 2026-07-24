package relayclient

import (
	"context"
	"math/rand/v2"
	"time"
)

type reconnectBackoff struct {
	attempt int
	min     time.Duration
	max     time.Duration
}

func newReconnectBackoff(minimum, maximum time.Duration) *reconnectBackoff {
	if minimum <= 0 {
		minimum = time.Second
	}
	if maximum < minimum {
		maximum = 30 * time.Second
	}
	return &reconnectBackoff{min: minimum, max: maximum}
}

func (backoff *reconnectBackoff) Next() time.Duration {
	delay := backoff.min
	for index := 0; index < backoff.attempt && delay < backoff.max/2; index++ {
		delay *= 2
	}
	if delay > backoff.max {
		delay = backoff.max
	}
	backoff.attempt++
	jitter := 0.8 + rand.Float64()*0.4
	result := time.Duration(float64(delay) * jitter)
	if result < backoff.min {
		return backoff.min
	}
	if result > backoff.max {
		return backoff.max
	}
	return result
}

func (backoff *reconnectBackoff) Reset() { backoff.attempt = 0 }

func waitReconnect(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
