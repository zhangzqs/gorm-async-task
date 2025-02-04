package gormasynctask

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 泛型异步任务表操作
type TaskTable[T TaskEntity] struct {
	db         *gorm.DB
	tableName  string
	taskIDName string
}

func NewTaskTable[T TaskEntity](db *gorm.DB) *TaskTable[T] {
	var t T
	return &TaskTable[T]{
		db:         db,
		tableName:  t.TableName(),
		taskIDName: t.GetTaskIDColumn(),
	}
}

// 批量创建任务
func (t *TaskTable[T]) Create(ctx context.Context, tasks ...T) error {
	// 任务ID不为空, 必须是init或pending状态
	for _, t := range tasks {
		if t.GetTaskID() == "" {
			return ErrEmptyTaskID
		}
		if t.GetState() != TaskStateInit && t.GetState() != TaskStatePending {
			return ErrNewTaskStateError
		}
	}
	err := t.db.WithContext(ctx).Create(tasks).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return ErrDuplicateTaskID
	}
	return nil
}

// 获取单个待处理任务（带悲观锁和重试机制）
func (t *TaskTable[T]) AcquireTask(
	ctx context.Context,
	zombieTaskTimeout time.Duration, // DOING任务的超时时间，超过该时间的任务即僵尸任务，通常为2~3倍的平均执行时长
) (task T, err error) {
	logger := LoggerProvider(ctx)
	const maxRetries = 30
	const baseDelay = 500 * time.Millisecond

	for i := 0; i < maxRetries; i++ {
		err = t.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			// 锁定并获取任务
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where(`(task_state = ?) OR 
					 (task_state = ? AND updated_at < ?) OR
                     (task_state = ? AND pending_until < ?)`,
					TaskStateInit,                                      // 就绪的任务
					TaskStateDoing, time.Now().Add(-zombieTaskTimeout), // 执行超时僵死的任务
					TaskStatePending, time.Now(), // 到达预期时间执行的任务
				).
				Order("created_at ASC").
				First(&task).Error; err != nil {
				return err
			}
			task.SetState(TaskStateDoing)     // 更新状态为处理中
			task.SetPendingUntil(time.Time{}) // 清空预计等待时间
			return tx.Save(&task).Error
		})

		if err == nil {
			return task, nil
		}

		if errors.Is(err, gorm.ErrRecordNotFound) {
			return task, ErrTaskNotFound
		}

		// 处理锁冲突
		if isLockError(err) {
			delay := time.Duration(i+1) * baseDelay
			logger.Warn("Database lock conflict, retrying...",
				zap.Int("attempt", i+1),
				zap.Duration("delay", delay))
			time.Sleep(delay)
			continue
		}

		return task, err
	}

	return task, ErrMaxRetriesReached
}

// 延迟任务
func (t *TaskTable[T]) MarkTaskPending(ctx context.Context, taskID string, updates map[string]any, delay time.Duration) error {
	if updates == nil {
		updates = make(map[string]any)
	}
	updates["task_state"] = TaskStatePending
	updates["updated_at"] = time.Now()
	updates["pending_until"] = time.Now().Add(delay)

	return t.db.WithContext(ctx).Model(new(T)).
		Where(t.taskIDName+" = ?", taskID).
		Updates(updates).Error
}

// 标记任务完成（可更新自定义字段）
func (t *TaskTable[T]) MarkTaskDone(ctx context.Context, taskID string, updates map[string]any) error {
	if updates == nil {
		updates = make(map[string]any)
	}
	updates["task_state"] = TaskStateDone
	updates["updated_at"] = time.Now()

	return t.db.WithContext(ctx).Model(new(T)).
		Where(t.taskIDName+" = ?", taskID).
		Updates(updates).Error
}

// 标记任务失败
func (t *TaskTable[T]) MarkTaskError(ctx context.Context, taskID string, err any) error {
	return t.db.WithContext(ctx).Model(new(T)).
		Where(t.taskIDName+" = ?", taskID).
		Updates(map[string]interface{}{
			"task_state":    TaskStateError,
			"error_message": fmt.Sprint(err),
			"updated_at":    time.Now(),
		}).Error
}

// 获取任务记录
func (t *TaskTable[T]) GetTaskByID(ctx context.Context, taskID string) (entity T, err error) {
	err = t.db.WithContext(ctx).
		Where(t.taskIDName+" = ? AND task_state = ?", taskID, TaskStateDone).
		First(&entity).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = ErrTaskNotFound
	}
	return
}

// 可以定期检查僵尸任务
func (t *TaskTable[T]) CheckZombieTasks(ctx context.Context, timeout time.Duration) ([]T, error) {
	var tasks []T
	err := t.db.WithContext(ctx).
		Where("(task_state = ? AND updated_at < ?) OR (task_state = ? AND pending_until < ?)",
			TaskStateDoing, time.Now().Add(-timeout),
			TaskStatePending, time.Now(),
		).
		Find(&tasks).Error
	return tasks, err
}

func (t *TaskTable[T]) GetStats(ctx context.Context) (map[TaskState]int64, error) {
	stats := make(map[TaskState]int64)

	// 先初始化所有状态为0
	for _, state := range []TaskState{
		TaskStateInit,
		TaskStateDoing,
		TaskStateDone,
		TaskStateError,
		TaskStatePending,
	} {
		stats[state] = 0
	}

	// 定义临时结构体用于接收查询结果
	type stateCount struct {
		State TaskState `gorm:"column:task_state"`
		Count int64     `gorm:"column:count"`
	}

	var results []stateCount

	// 执行分组查询
	err := t.db.WithContext(ctx).
		Model(new(T)).
		Select("task_state, count(*) as count").
		Group("task_state").
		Scan(&results).Error

	if err != nil {
		return nil, fmt.Errorf("failed to get task stats: %w", err)
	}

	// 合并查询结果到统计map
	for _, result := range results {
		stats[result.State] = result.Count
	}

	return stats, nil
}

func (t *TaskTable[T]) GetGormDB() *gorm.DB {
	return t.db
}
