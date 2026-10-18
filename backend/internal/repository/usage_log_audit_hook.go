package repository

import (
	"context"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/audit"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// auditUsageWritten 是请求归档的 OnUsage 钩子：把用量 request_id 关联到当前请求归档。
// 处于外部事务中的写入只标记为“未证实提交”，展示层始终以数据库中的 usage_logs 为准。
func auditUsageWritten(ctx context.Context, log *service.UsageLog, inserted bool, err error) {
	if log == nil || audit.FromContext(ctx) == nil {
		return
	}
	audit.OnUsage(ctx, audit.UsageEvent{
		RequestID: log.RequestID,
		APIKeyID:  log.APIKeyID,
		AccountID: log.AccountID,
		UserID:    log.UserID,
		Inserted:  inserted,
		InTx:      dbent.TxFromContext(ctx) != nil,
		Err:       err,
	})
}
