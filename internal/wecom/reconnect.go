package wecom

import (
	"context"
	"math"
	"math/rand/v2"
	"time"
)

const (
	defaultBackoffMin = time.Second
	defaultBackoffMax = 30 * time.Second
)

// Backoff 生成带有限抖动的指数重连等待时间。
//
// Backoff 仅由 Client 的 Run goroutine 使用，不支持并发调用。
type Backoff struct {
	attempt int
	min     time.Duration
	max     time.Duration
	random  func() float64
}

// NewBackoff 创建一个可注入随机源的重连退避器。
func NewBackoff(min, max time.Duration, random func() float64) *Backoff {
	if min <= 0 {
		min = defaultBackoffMin
	}
	if max < min {
		max = min
	}
	if random == nil {
		random = rand.Float64
	}
	return &Backoff{min: min, max: max, random: random}
}

// Next 返回下一次重连前应等待的时间，抖动范围为基准值的 80% 至 120%。
func (b *Backoff) Next() time.Duration {
	if b == nil {
		return defaultBackoffMin
	}
	base := b.min
	if b.attempt > 0 {
		shift := min(b.attempt, 62)
		if base > b.max/(1<<shift) {
			base = b.max
		} else {
			base *= 1 << shift
		}
	}
	if base > b.max {
		base = b.max
	}
	b.attempt++
	random := b.random()
	if random < 0 {
		random = 0
	}
	if random > 1 || math.IsNaN(random) {
		random = 1
	}
	factor := 0.8 + random*0.4
	delay := time.Duration(float64(base) * factor)
	minDelay := time.Duration(float64(base) * 0.8)
	maxDelay := time.Duration(float64(base) * 1.2)
	if delay < minDelay {
		return minDelay
	}
	if delay > maxDelay {
		delay = maxDelay
	}
	if delay > b.max {
		return b.max
	}
	return delay
}

// Reset 使下一次等待回到最小退避时间。
func (b *Backoff) Reset() {
	if b != nil {
		b.attempt = 0
	}
}

// WaitBackoff 在 context 仍有效时等待指定退避时间。
func WaitBackoff(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}
