package gormasynctask

import "strings"

// 辅助函数：判断是否为数据库锁错误
func isLockError(err error) bool {
	return strings.Contains(err.Error(), "database is locked") ||
		strings.Contains(err.Error(), "lock conflict")
}
