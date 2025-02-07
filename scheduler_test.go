package gormasynctask

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type TestTaskEntity struct {
	BaseTask
	TaskID    string `gorm:"column:task_id;primaryKey"`
	TestField string `gorm:"column:test_field"`
	Count     int    `gorm:"column:count"`
}

func (*TestTaskEntity) TableName() string {
	return "test_async_task"
}

func (*TestTaskEntity) GetTaskIDColumn() string {
	return "task_id"
}

func (e *TestTaskEntity) GetTaskID() string {
	return e.TaskID
}

func TestNormalService(t *testing.T) {
	dbDriver := sqlite.New(sqlite.Config{DSN: "test.db"})
	defer func() {
		_ = recover()
		os.Remove("test.db")
	}()

	db, err := gorm.Open(dbDriver, &gorm.Config{
		Logger: logger.Discard,
	})
	require.NoError(t, err)
	db.AutoMigrate(&TestTaskEntity{})

	taskTable := NewTaskTable[*TestTaskEntity](db)

	svr := NewTaskScheduler(
		taskTable,
		RunnerFunc[*TestTaskEntity](func(ctx context.Context, task *TestTaskEntity, handler Handler) {
			// 任务执行逻辑
			time.Sleep(1 * time.Second)

			if task.TaskID == "task-1" || task.TaskID == "task-4" {
				handler.Error(fmt.Errorf("task error %s", task.TaskID))
			} else {
				handler.DoneWithUpdater(map[string]any{"test_field": "testTaskDone"})
			}
		}),
	)

	ctx := context.Background()

	// 创建任务
	for i := 0; i < 100; i++ {
		taskTable.Create(ctx, &TestTaskEntity{
			BaseTask: BaseTask{TaskState: TaskStateInit},
			TaskID:   "task-" + fmt.Sprint(i),
		})
	}

	totalDone := []string{}
	totalError := []string{}
	totalErrorDetail := []error{}

	c := make(chan struct{}, 1)
	// 调度任务
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			output := svr.DoOnce(ctx, DoInput{
				Limit:             100,
				Concurrency:       50,
				ZombieTaskTimeout: 3 * time.Minute,
			})
			if output.Empty() {
				close(c)
				return
			}
			totalDone = append(totalDone, output.Done...)
			totalError = append(totalError, output.Error...)
			totalErrorDetail = append(totalErrorDetail, output.ErrorDetail...)
			time.Sleep(time.Millisecond * 500)
		}
	}()

	// 等待任务调度完毕
	<-c

	stats, err := taskTable.GetStats(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(98), stats[TaskStateDone])
	require.Equal(t, int64(2), stats[TaskStateError])

	require.Equal(t, 98, len(totalDone))
	require.Equal(t, 2, len(totalError))
	require.Len(t, totalDone, 98)
	require.Len(t, totalError, 2)
	require.Len(t, totalErrorDetail, 2)

	require.Contains(t, totalError, "task-1")
	require.Contains(t, totalError, "task-4")
	require.ErrorContains(t, errors.Join(totalErrorDetail...), "task error task-1")
	require.ErrorContains(t, errors.Join(totalErrorDetail...), "task error task-4")
}

func TestPending(t *testing.T) {
	dbDriver := sqlite.New(sqlite.Config{DSN: "test.db"})
	defer func() {
		_ = recover()
		os.Remove("test.db")
	}()

	db, err := gorm.Open(dbDriver, &gorm.Config{
		Logger: logger.Discard,
	})
	require.NoError(t, err)
	db.AutoMigrate(&TestTaskEntity{})

	taskTable := NewTaskTable[*TestTaskEntity](db)
	svr := NewTaskScheduler(
		taskTable,
		RunnerFunc[*TestTaskEntity](func(ctx context.Context, task *TestTaskEntity, handler Handler) {
			// 任务执行逻辑
			handler.DoneWithUpdater(map[string]any{"test_field": "testPendingTaskDone"})
		}),
	)
	ctx := context.Background()

	// 上来就直接创建一个定时任务
	err = taskTable.Create(ctx, &TestTaskEntity{
		BaseTask: BaseTask{
			TaskState: TaskStatePending,
			// 计划任务2s后执行
			PendingUntil: time.Now().Add(time.Second * 2),
		},
		TaskID: "task-pending-1",
	})
	require.NoError(t, err)

	go svr.Start(ctx, time.Millisecond*200, DoInput{
		Limit:       10,
		Concurrency: 10,
	})

	// 1s后任务执行不完，还处于pending
	time.Sleep(time.Second * 1)
	task, err := taskTable.GetTaskByID(ctx, "task-pending-1")
	require.NoError(t, err)
	require.Equal(t, TaskStatePending, task.GetState())

	// 3s后任务肯定执行完毕
	time.Sleep(time.Second * 3)
	task, err = taskTable.GetTaskByID(ctx, "task-pending-1")
	require.NoError(t, err)
	require.Equal(t, TaskStateDone, task.GetState())
	require.Equal(t, "testPendingTaskDone", task.TestField)
}

func TestHandleResultPending(t *testing.T) {
	dbDriver := sqlite.New(sqlite.Config{DSN: "test.db"})
	defer func() {
		_ = recover()
		os.Remove("test.db")
	}()

	db, err := gorm.Open(dbDriver, &gorm.Config{
		Logger: logger.Discard,
	})
	require.NoError(t, err)
	db.AutoMigrate(&TestTaskEntity{})

	taskTable := NewTaskTable[*TestTaskEntity](db)
	svr := NewTaskScheduler(
		taskTable,
		RunnerFunc[*TestTaskEntity](func(ctx context.Context, task *TestTaskEntity, handler Handler) {
			// 任务执行逻辑
			if task.Count < 3 {
				handler.PendingWithUpdater(map[string]any{"count": task.Count + 1}, time.Second)
			} else {
				handler.DoneWithUpdater(map[string]any{"test_field": "testPendingTaskDone"})
			}
		}),
	)
	ctx := context.Background()

	// 上来就直接创建一个定时任务
	err = taskTable.Create(ctx, &TestTaskEntity{
		BaseTask: BaseTask{TaskState: TaskStateInit},
		TaskID:   "task-pending-1",
	})
	require.NoError(t, err)

	doOnce := func() { svr.DoOnce(ctx, DoInput{Limit: 1, Concurrency: 1}) }

	assertStateAndCount := func(state TaskState, count int) {
		task, err := taskTable.GetTaskByID(ctx, "task-pending-1")
		require.NoError(t, err)
		require.Equal(t, state, task.GetState())
		require.Equal(t, count, task.Count)
	}

	assertStateAndCount(TaskStateInit, 0)
	doOnce()
	assertStateAndCount(TaskStatePending, 1)
	doOnce()
	assertStateAndCount(TaskStatePending, 1)
	time.Sleep(time.Second)
	doOnce()
	assertStateAndCount(TaskStatePending, 2)

	time.Sleep(time.Second)
	doOnce()
	assertStateAndCount(TaskStatePending, 3)

	time.Sleep(time.Second)
	doOnce()
	assertStateAndCount(TaskStateDone, 3)

}
