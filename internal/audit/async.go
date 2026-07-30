package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const (
	defaultMaxEvents      = 1024
	defaultMaxBytes       = 16 << 20
	defaultMaxBatchEvents = 64
	defaultMaxBatchBytes  = 512 << 10
	defaultFlushInterval  = time.Second
)

// BatchExporter 把一批审计事件写入一个实际输出目标。
type BatchExporter interface {
	Export(context.Context, []Event) error
}

// AsyncConfig 定义单个审计输出 worker 的有界资源参数。
type AsyncConfig struct {
	MaxEvents      int
	MaxBytes       int
	MaxBatchEvents int
	MaxBatchBytes  int
	FlushInterval  time.Duration
}

// AsyncStats 是异步审计输出的累计计数快照。
type AsyncStats struct {
	Enqueued uint64
	Exported uint64
	Dropped  uint64
	Failed   uint64
}

type queuedEvent struct {
	event Event
	bytes int
}

// AsyncAuditor 使用独立有界队列把 Router 与实际审计输出隔离。
type AsyncAuditor struct {
	exporter BatchExporter
	logger   *slog.Logger
	config   AsyncConfig
	queue    chan queuedEvent
	done     chan struct{}

	mu          sync.Mutex
	closed      bool
	queuedBytes int
	stats       AsyncStats
}

// NewAsyncAuditor 创建并启动一个独立审计输出 worker。
func NewAsyncAuditor(exporter BatchExporter, logger *slog.Logger, config AsyncConfig) (*AsyncAuditor, error) {
	if exporter == nil {
		return nil, errors.New("审计 exporter 无效")
	}
	config = normalizeAsyncConfig(config)
	auditor := &AsyncAuditor{
		exporter: exporter,
		logger:   logger,
		config:   config,
		queue:    make(chan queuedEvent, config.MaxEvents),
		done:     make(chan struct{}),
	}
	go auditor.run()
	return auditor, nil
}

func normalizeAsyncConfig(config AsyncConfig) AsyncConfig {
	if config.MaxEvents <= 0 {
		config.MaxEvents = defaultMaxEvents
	}
	if config.MaxBytes <= 0 {
		config.MaxBytes = defaultMaxBytes
	}
	if config.MaxBatchEvents <= 0 {
		config.MaxBatchEvents = defaultMaxBatchEvents
	}
	if config.MaxBatchBytes <= 0 {
		config.MaxBatchBytes = defaultMaxBatchBytes
	}
	if config.FlushInterval <= 0 {
		config.FlushInterval = defaultFlushInterval
	}
	return config
}

// Emit 尝试非阻塞入队；队列达到事件数或字节上限时丢弃新事件。
func (auditor *AsyncAuditor) Emit(event Event) {
	if auditor == nil {
		return
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		auditor.recordDrop()
		return
	}
	queued := queuedEvent{event: event, bytes: len(encoded)}
	auditor.mu.Lock()
	defer auditor.mu.Unlock()
	if auditor.closed || queued.bytes > auditor.config.MaxBytes || auditor.queuedBytes+queued.bytes > auditor.config.MaxBytes {
		auditor.stats.Dropped++
		auditor.logDropLocked()
		return
	}
	select {
	case auditor.queue <- queued:
		auditor.queuedBytes += queued.bytes
		auditor.stats.Enqueued++
	default:
		auditor.stats.Dropped++
		auditor.logDropLocked()
	}
}

// Stats 返回不包含正文的累计运行计数。
func (auditor *AsyncAuditor) Stats() AsyncStats {
	if auditor == nil {
		return AsyncStats{}
	}
	auditor.mu.Lock()
	defer auditor.mu.Unlock()
	return auditor.stats
}

// Shutdown 停止接收新事件并等待 worker 刷新已经接受的队列。
func (auditor *AsyncAuditor) Shutdown(ctx context.Context) error {
	if auditor == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	auditor.mu.Lock()
	if !auditor.closed {
		auditor.closed = true
		close(auditor.queue)
	}
	auditor.mu.Unlock()
	select {
	case <-auditor.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (auditor *AsyncAuditor) run() {
	defer close(auditor.done)
	ticker := time.NewTicker(auditor.config.FlushInterval)
	defer ticker.Stop()
	batch := make([]Event, 0, auditor.config.MaxBatchEvents)
	batchBytes := 0
	flush := func() {
		if len(batch) == 0 {
			return
		}
		events := append([]Event(nil), batch...)
		if err := auditor.exporter.Export(context.Background(), events); err != nil {
			auditor.mu.Lock()
			auditor.stats.Failed += uint64(len(events))
			auditor.mu.Unlock()
			if auditor.logger != nil {
				auditor.logger.Warn("审计事件输出失败", "error_type", "export", "event_count", len(events))
			}
		} else {
			auditor.mu.Lock()
			auditor.stats.Exported += uint64(len(events))
			auditor.mu.Unlock()
		}
		batch = batch[:0]
		batchBytes = 0
	}
	for {
		select {
		case queued, open := <-auditor.queue:
			if !open {
				flush()
				return
			}
			auditor.mu.Lock()
			auditor.queuedBytes -= queued.bytes
			auditor.mu.Unlock()
			if len(batch) > 0 && (len(batch) >= auditor.config.MaxBatchEvents || batchBytes+queued.bytes > auditor.config.MaxBatchBytes) {
				flush()
			}
			batch = append(batch, queued.event)
			batchBytes += queued.bytes
			if len(batch) >= auditor.config.MaxBatchEvents || batchBytes >= auditor.config.MaxBatchBytes {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (auditor *AsyncAuditor) recordDrop() {
	auditor.mu.Lock()
	auditor.stats.Dropped++
	auditor.logDropLocked()
	auditor.mu.Unlock()
}

func (auditor *AsyncAuditor) logDropLocked() {
	if auditor.logger != nil && (auditor.stats.Dropped == 1 || auditor.stats.Dropped%100 == 0) {
		auditor.logger.Warn("审计事件已丢弃", "error_type", "queue_full", "dropped_total", auditor.stats.Dropped)
	}
}

// MultiAuditor 把同一事件复制给多个互不阻塞的审计输出。
type MultiAuditor struct {
	auditors []Auditor
}

// NewMultiAuditor 创建组合审计器，并忽略 nil 输出。
func NewMultiAuditor(auditors ...Auditor) Auditor {
	filtered := make([]Auditor, 0, len(auditors))
	for _, auditor := range auditors {
		if auditor != nil {
			filtered = append(filtered, auditor)
		}
	}
	if len(filtered) == 0 {
		return NoopAuditor{}
	}
	if len(filtered) == 1 {
		return filtered[0]
	}
	return &MultiAuditor{auditors: filtered}
}

// Emit 把事件交给每个独立输出。
func (auditor *MultiAuditor) Emit(event Event) {
	for _, output := range auditor.auditors {
		output.Emit(event)
	}
}

// Shutdown 使用同一个截止时间关闭全部输出。
func (auditor *MultiAuditor) Shutdown(ctx context.Context) error {
	var shutdownErrors []error
	for index, output := range auditor.auditors {
		if err := output.Shutdown(ctx); err != nil {
			shutdownErrors = append(shutdownErrors, fmt.Errorf("关闭审计输出 %d: %w", index+1, err))
		}
	}
	return errors.Join(shutdownErrors...)
}
