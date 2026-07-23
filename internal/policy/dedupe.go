package policy

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// ErrInvalidDeduperConfig 表示幂等器的 TTL、容量或时钟无效。
var ErrInvalidDeduperConfig = errors.New("幂等器配置无效")

// Clock 返回当前时间，使幂等器可以在测试和运行时使用可控时钟。
type Clock func() time.Time

// Deduper 在任何 prompt 或 key 外部调用前登记 IM 消息幂等键。
//
// 登记成功后即使外部调用失败也不会删除该键；这样可以避免 webhook 重试重复输入。
type Deduper struct {
	mu       sync.Mutex
	ttl      time.Duration
	capacity int
	clock    Clock
	entries  map[string]dedupeEntry
	order    []orderEntry
	nextSeq  uint64
}

type dedupeEntry struct {
	expiresAt time.Time
	sequence  uint64
}

type orderEntry struct {
	key      string
	sequence uint64
}

// NewDeduper 创建容量受限、线程安全的消息幂等器。
func NewDeduper(ttl time.Duration, capacity int, clock Clock) (*Deduper, error) {
	if ttl <= 0 || capacity <= 0 || clock == nil {
		return nil, ErrInvalidDeduperConfig
	}
	return &Deduper{
		ttl:      ttl,
		capacity: capacity,
		clock:    clock,
		entries:  make(map[string]dedupeEntry, capacity),
		order:    make([]orderEntry, 0, capacity),
	}, nil
}

// AddIfNew 将非空幂等键登记为已处理；首次登记返回 true，重复键返回 false。
//
// 到达精确过期边界的键视为已过期。时钟回拨时，尚未到期的记录会保守地继续视为重复。
func (d *Deduper) AddIfNew(key string) bool {
	if d == nil || strings.TrimSpace(key) == "" {
		return false
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.clock()
	d.removeExpired(now)
	if _, exists := d.entries[key]; exists {
		return false
	}
	if len(d.entries) == d.capacity {
		oldest := d.order[0]
		entry := d.entries[oldest.key]
		if entry.sequence == oldest.sequence {
			delete(d.entries, oldest.key)
		}
		d.order = d.order[1:]
	}
	d.nextSeq++
	d.entries[key] = dedupeEntry{expiresAt: now.Add(d.ttl), sequence: d.nextSeq}
	d.order = append(d.order, orderEntry{key: key, sequence: d.nextSeq})
	return true
}

// removeExpired 同时压缩顺序队列，确保重插入不会由陈旧记录误删，且元数据有界。
func (d *Deduper) removeExpired(now time.Time) {
	kept := d.order[:0]
	for _, record := range d.order {
		entry, exists := d.entries[record.key]
		if !exists || entry.sequence != record.sequence {
			continue
		}
		if !entry.expiresAt.After(now) {
			delete(d.entries, record.key)
			continue
		}
		kept = append(kept, record)
	}
	d.order = kept
}

func (d *Deduper) entryCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries)
}

func (d *Deduper) orderCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.order)
}
