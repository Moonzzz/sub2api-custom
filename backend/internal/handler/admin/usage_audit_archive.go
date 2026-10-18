package admin

import (
	"context"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/audit"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// AuditArchiveUsageLookup 为请求归档提供用量记录查询（以数据库中已提交的行为准）。
func (h *UsageHandler) AuditArchiveUsageLookup() audit.UsageLookup {
	return func(ctx context.Context, id int64) (*audit.UsageRef, error) {
		if h == nil || h.usageService == nil {
			return nil, errors.New("usage service unavailable")
		}
		log, err := h.usageService.GetByID(ctx, id)
		if err != nil {
			if errors.Is(err, service.ErrUsageLogNotFound) {
				return nil, nil
			}
			return nil, err
		}
		if log == nil {
			return nil, nil
		}
		return &audit.UsageRef{
			ID:        log.ID,
			RequestID: log.RequestID,
			APIKeyID:  log.APIKeyID,
			AccountID: log.AccountID,
			UserID:    log.UserID,
			CreatedAt: log.CreatedAt,
		}, nil
	}
}
