package audit

import (
	"context"
	"strings"
	"time"
)

// 用量回调状态。注意：回调只说明“仓储层调用已返回”，不代表事务已提交；
// 展示层以数据库中实际存在的 usage_logs 行为准。
const (
	UsageStateWriteReturned = "write_returned"        // 非事务写入返回成功（仍以数据库查询为准）
	UsageStateDuplicate     = "duplicate_ignored"     // ON CONFLICT 未插入
	UsageStatePendingTx     = "pending_tx_not_proven" // 处于外部事务内，提交与否未知
	UsageStateFailed        = "write_failed"
)

// UsageEvent 是仓储层写用量后的回调参数。
type UsageEvent struct {
	RequestID string
	APIKeyID  int64
	AccountID int64
	UserID    int64
	Inserted  bool
	InTx      bool
	Err       error
}

// OnUsage 把用量 request_id 关联到当前请求的归档（追加索引 + 附注）。
// context 中没有归档器（后台补记、异步任务）时直接忽略。
func OnUsage(ctx context.Context, ev UsageEvent) {
	rec := FromContext(ctx)
	if rec == nil || strings.TrimSpace(ev.RequestID) == "" {
		return
	}
	state := UsageStateWriteReturned
	errText := ""
	switch {
	case ev.Err != nil:
		state, errText = UsageStateFailed, ev.Err.Error()
	case ev.InTx:
		state = UsageStatePendingTx
	case !ev.Inserted:
		state = UsageStateDuplicate
	}
	entry := IndexEntry{
		Time:           time.Now(),
		Via:            "usage_callback",
		RequestID:      rec.ID,
		Session:        rec.Session,
		UsageRequestID: ev.RequestID,
		UsageState:     state,
		UsageError:     errText,
		APIKeyID:       ev.APIKeyID,
		AccountID:      ev.AccountID,
		UserID:         ev.UserID,
	}
	// 请求进行中：暂存到归档器，由落盘 goroutine 写索引（不在请求路径上做文件 I/O）；
	// 请求已结束（异步计费回调）：后台补写。
	rec.mu.Lock()
	if !rec.finished.Load() {
		rec.usage = append(rec.usage, entry)
		rec.mu.Unlock()
	} else {
		rec.mu.Unlock()
		rec.store.pending.Add(1)
		go func() {
			defer rec.store.pending.Done()
			if _, err := rec.ensureFiles(); err == nil {
				entry.Ref = rec.ArchiveRef()
				rec.store.writeUsageIndex(entry)
			}
		}()
	}
	rec.Note(KindClientResponse, 0, map[string]any{
		"type":             "usage_callback",
		"usage_request_id": ev.RequestID,
		"usage_state":      state,
		"usage_error":      errText,
		"api_key_id":       ev.APIKeyID,
		"account_id":       ev.AccountID,
	})
}

// writeUsageIndex 用量 request_id 与客户端请求 ID 不同（client:/local:/web_search: 等前缀）时单独建索引。
func (s *Store) writeUsageIndex(e IndexEntry) {
	if e.UsageRequestID != e.RequestID {
		s.appendIndex(e.UsageRequestID, e)
	}
	s.appendIndex(e.RequestID, e)
}
