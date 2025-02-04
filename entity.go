package gormasynctask

import (
	"errors"
	"time"
)

type TaskState string

const (
	TaskStateInit    TaskState = "INIT"
	TaskStateDoing   TaskState = "DOING"
	TaskStateDone    TaskState = "DONE"
	TaskStateError   TaskState = "ERROR"
	TaskStatePending TaskState = "PENDING"
)

var (
	ErrDuplicateTaskID   = errors.New("duplicate taskID")
	ErrEmptyTaskID       = errors.New("empty task id for new task")
	ErrNewTaskStateError = errors.New("new task must be init state")
	ErrTaskNotFound      = errors.New("task not found")
	ErrLockConflict      = errors.New("database lock conflict")
	ErrMaxRetriesReached = errors.New("max retries reached")
)

// 基础任务实体结构体（需嵌入到具体任务实体中）
type BaseTask struct {
	CreatedAt    time.Time `gorm:"autoCreateTime"`
	UpdatedAt    time.Time `gorm:"autoUpdateTime"`
	TaskState    TaskState `gorm:"column:task_state;index"`
	PendingUntil time.Time `gorm:"column:pending_until;index"`
	ErrorMessage string    `gorm:"column:error_message;type:varchar(500)"`
}

// 任务实体接口约束
type TaskEntity interface {
	TableName() string
	GetTaskIDColumn() string
	GetTaskID() string

	GetState() TaskState
	SetState(TaskState)
	GetPendingUntil() time.Time
	SetPendingUntil(time.Time)
}

func (t *BaseTask) GetState() TaskState { return t.TaskState }

func (t *BaseTask) SetState(s TaskState) { t.TaskState = s }

func (b *BaseTask) GetPendingUntil() time.Time { return b.PendingUntil }

func (b *BaseTask) SetPendingUntil(t time.Time) { b.PendingUntil = t }
