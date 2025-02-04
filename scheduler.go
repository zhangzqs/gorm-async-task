package gormasynctask

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"
)

type Handler interface {
	Done()
	DoneWithUpdater(updater map[string]any)
	Pending(delayTime time.Duration)
	PendingWithUpdater(updater map[string]any, delayTime time.Duration)
	Error(err error)
}

type handlerImpl struct {
	state     TaskState
	updater   map[string]any
	delayTime time.Duration
	err       error
}

func (h *handlerImpl) DoneWithUpdater(updater map[string]any) {
	h.state = TaskStateDone
	h.updater = updater
}

func (h *handlerImpl) PendingWithUpdater(updater map[string]any, delayTime time.Duration) {
	h.state = TaskStatePending
	h.updater = updater
	h.delayTime = delayTime
}

func (h *handlerImpl) Done() {
	h.DoneWithUpdater(nil)
}

func (h *handlerImpl) Pending(delayTime time.Duration) {
	h.PendingWithUpdater(nil, 0)
}

func (h *handlerImpl) Error(err error) {
	if err == nil {
		panic("unexpected to error state with nil error")
	}
	h.state = TaskStateError
	h.err = err
}

type Runner[T TaskEntity] interface {
	Run(ctx context.Context, task T, taskHandler Handler)
}

type RunnerFunc[T TaskEntity] func(ctx context.Context, task T, taskHandler Handler)

func (f RunnerFunc[T]) Run(ctx context.Context, task T, taskHandler Handler) {
	f(ctx, task, taskHandler)
}

type DoInput struct {
	Limit             int           // 本次最多执行多少条任务(但是即使数据库中存在超过Limit条可执行的任务，依旧不保证本次一定会执行这么多任务)
	Concurrency       int           // 本次执行任务的并发度是多少
	ZombieTaskTimeout time.Duration // 僵尸任务的超时时间判定
}

type DoOutput struct {
	Done        []string `json:"done"`    // 已完成任务
	Error       []string `json:"error"`   // 出错的任务
	ErrorDetail []error  `json:"-"`       // 出错的任务细节
	Pending     []string `json:"pending"` // 继续等待的任务
}

func (o *DoOutput) Empty() bool {
	return len(o.Done) == 0 && len(o.Error) == 0 && len(o.ErrorDetail) == 0
}

type TaskScheduler[T TaskEntity] struct {
	table  *TaskTable[T] // 任务表
	runner Runner[T]
}

func NewTaskScheduler[T TaskEntity](table *TaskTable[T], runner Runner[T]) *TaskScheduler[T] {
	return &TaskScheduler[T]{
		table:  table,
		runner: runner,
	}
}

func (s TaskScheduler[T]) Start(ctx context.Context, d time.Duration, input DoInput) {
	ticker := time.NewTicker(d)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			output := s.DoOnce(ctx, input)
			LoggerProvider(ctx).Debug("ticker do once result", zap.Any("doOnceOutput", output))
		case <-ctx.Done():
			return
		}
	}
}

func (s TaskScheduler[T]) DoOnce(ctx context.Context, input DoInput) (output DoOutput) {
	if input.ZombieTaskTimeout == 0 {
		input.ZombieTaskTimeout = time.Hour
	}
	// 使用 channel 来收集处理结果
	doneChan := make(chan string, input.Limit)

	type failedPair struct {
		taskId string
		err    error
	}
	failedChan := make(chan failedPair, input.Limit)
	pendingChan := make(chan string, input.Limit)

	// 使用 WaitGroup 来等待所有 goroutine 完成
	var wg sync.WaitGroup

	// 限制并发度
	sem := make(chan struct{}, input.Concurrency)
	// 启动多个 goroutine 并发处理任务
	for i := 0; i < input.Limit; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// 限制并发数
			sem <- struct{}{}        // 获取信号量
			defer func() { <-sem }() // 释放信号量

			taskId, result := s.doOne(ctx, input.ZombieTaskTimeout)
			if taskId == "" && result == nil {
				// 空id表示没有能领取到待执行的任务，即任务根本就没执行，直接结束这个任务
				return
			}
			switch result.state {
			case TaskStateDone:
				doneChan <- taskId
			case TaskStateError:
				failedChan <- failedPair{taskId: taskId, err: result.err}
			case TaskStatePending:
				pendingChan <- taskId
			}
		}()
	}

	// 启动一个 goroutine 来关闭 channel，当所有任务完成时
	go func() {
		wg.Wait()
		close(doneChan)
		close(failedChan)
		close(pendingChan)
	}()

	// 收集处理结果，把空任务过滤掉
	for taskId := range doneChan {
		output.Done = append(output.Done, taskId)
	}
	for taskId := range pendingChan {
		output.Pending = append(output.Pending, taskId)
	}
	for pair := range failedChan {
		output.Error = append(output.Error, pair.taskId)
		output.ErrorDetail = append(output.ErrorDetail, pair.err)
	}
	return
}

func (s TaskScheduler[T]) doOne(ctx context.Context, zombieTaskTimeout time.Duration) (taskID string, handler *handlerImpl) {
	logger := LoggerProvider(ctx)

	task, err := s.table.AcquireTask(ctx, zombieTaskTimeout)
	if err != nil {
		if errors.Is(err, ErrTaskNotFound) {
			// 任务执行完毕，找不到可以执行的任务了
			logger.Debug("no task need to do")
			return
		}
		if err = s.table.MarkTaskError(ctx, task.GetTaskID(), err); err != nil {
			logger.Error("mark task error", zap.Error(err))
			return
		}
	}

	taskID = task.GetTaskID()                    // 拿到了任务ID
	handler = &handlerImpl{state: TaskStateDone} // 构造出任务状态控制器，默认是Done状态

	defer func() {
		// panic的任务标记为error状态
		if t := recover(); t != nil {
			if err := s.table.MarkTaskError(ctx, taskID, t); err != nil {
				logger.Error("mark task error", zap.Error(err))
				return
			}
		}
	}()

	// 任务执行逻辑
	s.runner.Run(ctx, task, handler)

	switch handler.state {
	case TaskStateDone:
		err = s.table.MarkTaskDone(ctx, taskID, handler.updater)
	case TaskStateError:
		err = s.table.MarkTaskError(ctx, taskID, handler.err)
	case TaskStatePending:
		err = s.table.MarkTaskPending(ctx, taskID, handler.updater, handler.delayTime)
	default:
		panic("unexpected state: " + handler.state)
	}
	if err != nil {
		logger.Error("mark task error", zap.Error(err))
		return
	}
	return
}
