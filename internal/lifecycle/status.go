// Package lifecycle 管理 Herdr Pal 的本地启动、守护和退出生命周期。
package lifecycle

import "sync"

// State 表示 Pal Supervisor 当前所处的生命周期阶段。
type State string

const (
	// StateStarting 表示 Supervisor 正在完成初始探测和 Worker 启动。
	StateStarting State = "starting"
	// StateRunning 表示 Worker 已运行且 Herdr 当前可连接。
	StateRunning State = "running"
	// StateHerdrGrace 表示 Herdr 暂时不可连接，正在退出宽限期内探测。
	StateHerdrGrace State = "herdr_grace"
	// StateWorkerBackoff 表示 Herdr 存活，但 Worker 正在等待重启。
	StateWorkerBackoff State = "worker_backoff"
	// StateStopping 表示 Supervisor 正在停止 Worker 并清理运行资源。
	StateStopping State = "stopping"
)

// HerdrState 表示 Supervisor 对 Herdr Server 存活和兼容性的判断。
type HerdrState string

const (
	// HerdrUnknown 表示尚未完成第一次 Herdr 探测。
	HerdrUnknown HerdrState = "unknown"
	// HerdrHealthy 表示 Herdr 可连接且公开协议兼容。
	HerdrHealthy HerdrState = "healthy"
	// HerdrIncompatible 表示 Herdr 可连接但公开协议不兼容。
	HerdrIncompatible HerdrState = "incompatible"
	// HerdrUnavailable 表示 Herdr 暂时不可连接。
	HerdrUnavailable HerdrState = "unavailable"
)

// Status 是本地健康端点公开的非敏感 Supervisor 状态。
type Status struct {
	State     State      `json:"state"`
	Herdr     HerdrState `json:"herdr"`
	WorkerPID int        `json:"worker_pid,omitempty"`
}

// StatusStore 为 Supervisor 的并发组件提供原子状态快照和字段更新。
type StatusStore struct {
	mu     sync.RWMutex
	status Status
}

// NewStatusStore 使用 initial 创建状态存储。
func NewStatusStore(initial Status) *StatusStore {
	return &StatusStore{status: initial}
}

// Load 返回当前状态的值拷贝。
func (store *StatusStore) Load() Status {
	if store == nil {
		return Status{}
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.status
}

// Update 在同一临界区内修改当前状态，避免并发组件覆盖彼此字段。
func (store *StatusStore) Update(update func(*Status)) {
	if store == nil || update == nil {
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	update(&store.status)
}
