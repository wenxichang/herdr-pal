package policy

import (
	"container/list"
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
	order    *list.List
	lastNow  time.Time
	hasNow   bool
}

type dedupeEntry struct {
	expiresAt time.Time
	element   *list.Element
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
		order:    list.New(),
	}, nil
}

// AddIfNew 将非空幂等键登记为已处理；首次登记返回 true，重复键返回 false。
//
// nil 接收者、空白键和重复键均返回 false。到达精确过期边界的键视为已过期；时钟回拨时
// 使用锁内单调逻辑时间，因此尚未到期的记录会保守地继续视为重复。
func (d *Deduper) AddIfNew(key string) bool {
	if d == nil || strings.TrimSpace(key) == "" {
		return false
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.logicalNow()
	d.removeExpired(now)
	if _, exists := d.entries[key]; exists {
		return false
	}
	if len(d.entries) == d.capacity {
		d.removeOldest()
	}
	element := d.order.PushBack(key)
	d.entries[key] = dedupeEntry{expiresAt: now.Add(d.ttl), element: element}
	return true
}

func (d *Deduper) logicalNow() time.Time {
	raw := d.clock()
	if !d.hasNow || raw.After(d.lastNow) {
		d.lastNow = raw
		d.hasNow = true
	}
	return d.lastNow
}

// removeExpired 仅从队首清理；逻辑时间和过期时间均单调不减，因此该操作摊销为 O(1)。
func (d *Deduper) removeExpired(now time.Time) {
	for element := d.order.Front(); element != nil; element = d.order.Front() {
		key := element.Value.(string)
		entry := d.entries[key]
		if entry.expiresAt.After(now) {
			return
		}
		delete(d.entries, key)
		d.order.Remove(element)
	}
}

func (d *Deduper) removeOldest() {
	element := d.order.Front()
	key := element.Value.(string)
	delete(d.entries, key)
	d.order.Remove(element)
}

func (d *Deduper) entryCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries)
}

func (d *Deduper) orderCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.order.Len()
}
