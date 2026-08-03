package lifecycle

import (
	"errors"
	"sync"
	"time"
)

// ErrInvalidBackoff 表示退避上下界无法组成有效序列。
var ErrInvalidBackoff = errors.New("Pal 生命周期退避配置无效")

// Backoff 提供线程安全、有上限的确定性指数退避。
type Backoff struct {
	mu      sync.Mutex
	minimum time.Duration
	maximum time.Duration
	next    time.Duration
}

// NewBackoff 创建从 minimum 开始且不超过 maximum 的指数退避。
func NewBackoff(minimum, maximum time.Duration) (*Backoff, error) {
	if minimum <= 0 || maximum < minimum {
		return nil, ErrInvalidBackoff
	}
	return &Backoff{minimum: minimum, maximum: maximum, next: minimum}, nil
}

// Next 返回本次等待时长，并推进到下一退避级别。
func (backoff *Backoff) Next() time.Duration {
	backoff.mu.Lock()
	defer backoff.mu.Unlock()
	result := backoff.next
	if backoff.next >= backoff.maximum/2 {
		backoff.next = backoff.maximum
	} else {
		backoff.next *= 2
	}
	return result
}

// Reset 把下一次等待恢复为最小值。
func (backoff *Backoff) Reset() {
	backoff.mu.Lock()
	defer backoff.mu.Unlock()
	backoff.next = backoff.minimum
}
