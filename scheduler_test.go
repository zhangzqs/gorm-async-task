package gormasynctask

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type TestTaskEntity struct {
	BaseTask
	TaskID    string `gorm:"column:task_id;primaryKey"`
	TestField string `gorm:"column:test_field"`
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

func TestService(t *testing.T) {
	dbDriver := sqlite.New(sqlite.Config{DSN: "test.db"})
	defer func() {
		_ = recover()
		os.Remove("test.db")
	}()

	db, err := gorm.Open(dbDriver, &gorm.Config{
		Logger: logger.Discard,
	})
	assert.NoError(t, err)
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

	assert.Equal(t, 98, len(totalDone))
	assert.Equal(t, 2, len(totalError))
	assert.Len(t, totalDone, 98)
	assert.Len(t, totalError, 2)
	assert.Len(t, totalErrorDetail, 2)

	assert.Contains(t, totalError, "task-1")
	assert.Contains(t, totalError, "task-4")
	assert.ErrorContains(t, errors.Join(totalErrorDetail...), "task error task-1")
	assert.ErrorContains(t, errors.Join(totalErrorDetail...), "task error task-4")
}
