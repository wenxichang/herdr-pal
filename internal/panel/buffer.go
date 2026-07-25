package panel

import "errors"

const (
	// PageSize 是一页终端快照包含的逻辑行数。
	PageSize = 100
	// MaxLines 是可在本地分页访问的终端快照行数上限。
	MaxLines = 1000
)

var (
	// ErrPanelChanged 表示扩大读取后的快照无法与现有缓存可靠对齐。
	ErrPanelChanged = errors.New("终端内容已变化")
	// ErrNewestPage 表示已经处于最新一页。
	ErrNewestPage = errors.New("已经是最新内容")
	// ErrOldestPage 表示已经没有可读取的更早内容。
	ErrOldestPage = errors.New("已经是最早可读取内容")
)

// Buffer 保存单个已选择目标的终端快照分页缓存。
type Buffer struct {
	targetKey string
	lines     []string
	page      int
	oldest    bool
	newestLen int
}

// Refresh 替换为最新 100 行并回到第 0 页。
func (b *Buffer) Refresh(targetKey string, lines []string) {
	if len(lines) > PageSize {
		lines = lines[len(lines)-PageSize:]
	}
	b.targetKey = targetKey
	b.lines = append([]string(nil), lines...)
	b.page = 0
	b.oldest = false
	b.newestLen = len(b.lines)
}

// NextReadSize 返回下一次 pageup 需要的 agent.read lines。
//
// 即使下一页已在缓存中，仍返回受 MaxLines 限制的正数，保持调用方“读取后调用 Expand”
// 的流程不变；Expand 会优先消费缓存页。
func (b *Buffer) NextReadSize() (int, error) {
	normalSize := min((b.page+2)*PageSize, MaxLines)
	if b.hasCachedOlderPage() {
		return max(normalSize, len(b.lines)), nil
	}
	if b.oldest || (b.page+2)*PageSize > MaxLines {
		return 0, ErrOldestPage
	}
	return normalSize, nil
}

// Expand 用旧缓存作为连续重叠锚点扩充更早内容。
//
// 同一缓存可能在新快照中重复出现，因此始终使用最后一次完整连续匹配，避免把较早的
// 重复输出误认为最新缓存。锚点后的内容是读取期间产生的新输出，不进入当前分页缓存。
func (b *Buffer) Expand(targetKey string, snapshot []string) error {
	if targetKey != b.targetKey || len(b.lines) == 0 {
		b.Reset()
		return ErrPanelChanged
	}
	if len(snapshot) > MaxLines {
		snapshot = snapshot[len(snapshot)-MaxLines:]
	}
	anchor := lastContiguousIndex(snapshot, b.lines)
	if anchor < 0 {
		if b.hasCachedOlderPage() && isRolledForward(snapshot, b.lines) {
			b.page++
			return nil
		}
		b.Reset()
		return ErrPanelChanged
	}
	prefix := snapshot[:anchor]
	room := MaxLines - len(b.lines)
	if len(prefix) > room {
		prefix = prefix[len(prefix)-room:]
	}
	if len(prefix) == 0 {
		if b.hasCachedOlderPage() {
			b.page++
			return nil
		}
		b.oldest = true
		return ErrOldestPage
	}
	b.lines = append(append([]string(nil), prefix...), b.lines...)
	b.page++
	b.oldest = len(b.lines) >= MaxLines
	return nil
}

// PageDown 向更新内容移动一页，不读取或刷新终端。
func (b *Buffer) PageDown() error {
	if b.page == 0 {
		return ErrNewestPage
	}
	b.page--
	return nil
}

// Render 返回当前页的副本。
func (b *Buffer) Render() []string {
	if len(b.lines) == 0 {
		return nil
	}
	newestLen := b.latestPageLen()
	end := len(b.lines)
	if b.page > 0 {
		end -= newestLen + (b.page-1)*PageSize
	}
	if end <= 0 {
		return nil
	}
	pageSize := PageSize
	if b.page == 0 {
		pageSize = newestLen
	}
	start := end - pageSize
	if start < 0 {
		start = 0
	}
	return append([]string(nil), b.lines[start:end]...)
}

// PagePosition 返回从最新页开始、从 1 起算的页码和当前缓存总页数。
func (b *Buffer) PagePosition() (current, total int) {
	if len(b.lines) == 0 {
		return 0, 0
	}
	olderLines := len(b.lines) - b.latestPageLen()
	total = 1 + (olderLines+PageSize-1)/PageSize
	return b.page + 1, total
}

func (b *Buffer) hasCachedOlderPage() bool {
	return len(b.lines)-b.latestPageLen() > b.page*PageSize
}

func (b *Buffer) latestPageLen() int {
	if b.newestLen <= 0 || b.newestLen > len(b.lines) {
		return min(PageSize, len(b.lines))
	}
	return b.newestLen
}

// Reset 清空全部分页状态。
func (b *Buffer) Reset() {
	b.targetKey = ""
	b.lines = nil
	b.page = 0
	b.oldest = false
	b.newestLen = 0
}

func lastContiguousIndex(snapshot, anchor []string) int {
	if len(anchor) == 0 || len(anchor) > len(snapshot) {
		return -1
	}
	for index := len(snapshot) - len(anchor); index >= 0; index-- {
		matched := true
		for offset, line := range anchor {
			if snapshot[index+offset] != line {
				matched = false
				break
			}
		}
		if matched {
			return index
		}
	}
	return -1
}

func isRolledForward(snapshot, previous []string) bool {
	if len(snapshot) != MaxLines || len(previous) != MaxLines {
		return false
	}
	for overlap := MaxLines - 1; overlap >= PageSize; overlap-- {
		matched := true
		for index := 0; index < overlap; index++ {
			if snapshot[index] != previous[len(previous)-overlap+index] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}
