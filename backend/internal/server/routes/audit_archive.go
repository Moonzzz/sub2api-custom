package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/audit"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// withAuditArchive 在 ClientRequestID 中间件外层接入请求报文归档（internal/audit）。
// 归档关闭时 audit.WrapHandler 直接调用原中间件，不改变任何行为。
func withAuditArchive(next gin.HandlerFunc) gin.HandlerFunc {
	return audit.WrapHandler(next, auditArchiveIdentity)
}

// auditArchiveIdentity 在请求结束时提取 API Key / 用户 / 分组 / 账户等关联字段。
func auditArchiveIdentity(c *gin.Context) map[string]any {
	meta := map[string]any{}
	if v, ok := c.Get(string(middleware.ContextKeyAPIKey)); ok {
		if key, ok := v.(*service.APIKey); ok && key != nil {
			meta["api_key_id"] = key.ID
			meta["user_id"] = key.UserID
			if key.GroupID != nil {
				meta["group_id"] = *key.GroupID
			}
		}
	}
	if v, ok := c.Get("ops_account_id"); ok {
		meta["account_id"] = v
	}
	if v, ok := c.Get("ops_model"); ok {
		meta["model"] = v
	}
	return meta
}

// registerAuditArchiveRoutes 注册归档查看接口（已挂在管理员鉴权之后）。
func registerAuditArchiveRoutes(admin *gin.RouterGroup, h *handler.Handlers) {
	var lookup audit.UsageLookup
	if h != nil && h.Admin != nil && h.Admin.Usage != nil {
		lookup = h.Admin.Usage.AuditArchiveUsageLookup()
	}
	audit.RegisterRoutes(admin, lookup)
}
