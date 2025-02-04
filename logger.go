package gormasynctask

import (
	"context"

	"go.uber.org/zap"
)

var LoggerProvider func(ctx context.Context) *zap.Logger = func(ctx context.Context) *zap.Logger {
	logger, _ := zap.NewDevelopment()
	return logger
}
