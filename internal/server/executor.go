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
	ctx    context.Context
	run    func(context.Context) error
	result chan error
}

type userQueue struct {
	tasks   chan userTask
	pending int
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
	if executor == nil || strings.TrimSpace(userID) == "" || run == nil {
		return ErrInvalidUserTask
	}
	if ctx == nil {
		ctx = context.Background()
	}
	task := userTask{ctx: ctx, run: run, result: make(chan error, 1)}
	executor.mu.Lock()
	queue := executor.users[userID]
	if queue == nil {
		queue = &userQueue{tasks: make(chan userTask, executor.capacity)}
		executor.users[userID] = queue
	}
	if queue.pending >= executor.capacity {
		executor.mu.Unlock()
		return ErrUserQueueFull
	}
	queue.pending++
	queue.tasks <- task
	if queue.pending == 1 {
		go executor.runUser(userID, queue)
	}
	executor.mu.Unlock()
	return <-task.result
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
		task.result <- err

		executor.mu.Lock()
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
