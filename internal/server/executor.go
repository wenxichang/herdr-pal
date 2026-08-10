package server

import (
	"context"
	"errors"
	"strings"
	"sync"
)

var (
	// ErrUserQueueFull 表示该用户的有界串行队列已满。
	ErrUserQueueFull = errors.New("用户输入队列已满")
	// ErrInvalidUserTask 表示任务缺少 userid 或执行函数。
	ErrInvalidUserTask = errors.New("用户任务无效")
)

type userTask struct {
	ctx      context.Context
	run      func(context.Context) error
	result   chan error
	overflow bool
}

type userQueue struct {
	tasks           chan userTask
	pending         int
	overflowPending bool
}

// UserExecutor 按 userid 串行执行任务，不同 userid 使用独立 worker。
type UserExecutor struct {
	mu       sync.Mutex
	capacity int
	users    map[string]*userQueue
}

// NewUserExecutor 创建单用户最多 capacity 个在途任务的执行器。
func NewUserExecutor(capacity int) *UserExecutor {
	if capacity <= 0 {
		capacity = 64
	}
	return &UserExecutor{capacity: capacity, users: make(map[string]*userQueue)}
}

// Submit 排队执行任务，并等待任务完成或返回安全队列错误。
func (executor *UserExecutor) Submit(ctx context.Context, userID string, run func(context.Context) error) error {
	result := make(chan error, 1)
	if err := executor.enqueue(ctx, userID, run, result, false); err != nil {
		return err
	}
	return <-result
}

// Enqueue 按 userid 排队执行任务，并在成功入队后立即返回。
func (executor *UserExecutor) Enqueue(ctx context.Context, userID string, run func(context.Context) error) error {
	return executor.enqueue(ctx, userID, run, nil, false)
}

// EnqueueOverflow 为队列已满的用户保留一个有序过载通知任务。
func (executor *UserExecutor) EnqueueOverflow(ctx context.Context, userID string, run func(context.Context) error) error {
	return executor.enqueue(ctx, userID, run, nil, true)
}

func (executor *UserExecutor) enqueue(ctx context.Context, userID string, run func(context.Context) error, result chan error, overflow bool) error {
	if executor == nil || strings.TrimSpace(userID) == "" || run == nil {
		return ErrInvalidUserTask
	}
	if ctx == nil {
		ctx = context.Background()
	}
	task := userTask{ctx: ctx, run: run, result: result, overflow: overflow}
	executor.mu.Lock()
	queue := executor.users[userID]
	if queue == nil {
		queue = &userQueue{tasks: make(chan userTask, executor.capacity+1)}
		executor.users[userID] = queue
	}
	if overflow && queue.overflowPending || !overflow && queue.pending >= executor.capacity {
		executor.mu.Unlock()
		return ErrUserQueueFull
	}
	if overflow {
		queue.overflowPending = true
	}
	queue.pending++
	queue.tasks <- task
	if queue.pending == 1 {
		go executor.runUser(userID, queue)
	}
	executor.mu.Unlock()
	return nil
}

func (executor *UserExecutor) runUser(userID string, queue *userQueue) {
	for {
		task := <-queue.tasks
		var err error
		if task.ctx.Err() != nil {
			err = task.ctx.Err()
		} else {
			err = task.run(task.ctx)
		}
		if task.result != nil {
			task.result <- err
		}

		executor.mu.Lock()
		if task.overflow {
			queue.overflowPending = false
		}
		queue.pending--
		if queue.pending == 0 {
			if executor.users[userID] == queue {
				delete(executor.users, userID)
			}
			executor.mu.Unlock()
			return
		}
		executor.mu.Unlock()
	}
}
