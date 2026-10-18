package audit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

// UsageRef 是用量记录中与归档关联的字段（由装配层从数据库读取）。
type UsageRef struct {
	ID        int64     `json:"id"`
	RequestID string    `json:"request_id"`
	APIKeyID  int64     `json:"api_key_id"`
	AccountID int64     `json:"account_id"`
	UserID    int64     `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
}

// UsageLookup 按 ID 读取用量记录；不存在时返回 (nil, nil)。
type UsageLookup func(ctx context.Context, id int64) (*UsageRef, error)

// Candidate 是一条候选归档。
type Candidate struct {
	Ref         string       `json:"ref"`
	Entries     []IndexEntry `json:"entries"`
	Score       int          `json:"score"`
	MatchReason []string     `json:"match_reason"`
}

// RegisterRoutes 注册管理端归档接口（调用方负责挂在管理员鉴权之后）。
func RegisterRoutes(g *gin.RouterGroup, lookup UsageLookup) {
	a := g.Group("/audit-archive")
	a.GET("/status", handleStatus)
	a.GET("/usage/:id", func(c *gin.Context) { handleUsage(c, lookup) })
	a.GET("/lookup", handleLookup)
	a.GET("/archive", handleArchive)
	a.GET("/body", handleBody)
}

func storeOr503(c *gin.Context) *Store {
	s := Default()
	if s == nil {
		response.ErrorWithDetails(c, http.StatusServiceUnavailable, "request archive is disabled", "AUDIT_ARCHIVE_DISABLED", nil)
		return nil
	}
	return s
}

func handleStatus(c *gin.Context) {
	s := Default()
	if s == nil {
		response.Success(c, gin.H{"enabled": false})
		return
	}
	response.Success(c, gin.H{"enabled": true, "root": s.Root(), "stats": s.StatsSnapshot(), "display_body_limit": s.cfg.DisplayBodyLimit})
}

// lookupKeys 用量 request_id 通常为 "client:<客户端请求ID>"；两者都查。
func lookupKeys(id string) []string {
	id = strings.TrimSpace(id)
	keys := []string{id}
	if rest, ok := strings.CutPrefix(id, "client:"); ok && rest != "" {
		keys = append(keys, rest)
	}
	return keys
}

// Resolve 汇总候选归档并按关联证据打分。
func (s *Store) Resolve(u *UsageRef) ([]Candidate, error) {
	byRef := map[string]*Candidate{}
	var order []string
	for _, key := range lookupKeys(u.RequestID) {
		entries, err := s.Lookup(key)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			cand, ok := byRef[e.Ref]
			if !ok {
				cand = &Candidate{Ref: e.Ref}
				byRef[e.Ref] = cand
				order = append(order, e.Ref)
			}
			cand.Entries = append(cand.Entries, e)
		}
	}
	out := make([]Candidate, 0, len(order))
	for _, ref := range order {
		cand := byRef[ref]
		reasons := map[string]bool{}
		for _, e := range cand.Entries {
			if e.Via == "usage_callback" && e.UsageRequestID == u.RequestID {
				reasons["usage_callback_request_id"] = true
				if u.APIKeyID != 0 && e.APIKeyID == u.APIKeyID {
					reasons["api_key_id"] = true
				}
				if u.AccountID != 0 && e.AccountID == u.AccountID {
					reasons["account_id"] = true
				}
			}
			if e.Via == "client_request_id" {
				reasons["client_request_id"] = true
				if !u.CreatedAt.IsZero() {
					d := u.CreatedAt.Sub(e.Time)
					if d >= -time.Minute && d <= 6*time.Hour {
						reasons["time_window"] = true
					}
				}
			}
		}
		for r := range reasons {
			cand.MatchReason = append(cand.MatchReason, r)
			cand.Score++
			if r == "usage_callback_request_id" {
				cand.Score += 2
			}
		}
		out = append(out, *cand)
	}
	return out, nil
}

func handleUsage(c *gin.Context, lookup UsageLookup) {
	s := storeOr503(c)
	if s == nil {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "invalid usage id")
		return
	}
	if lookup == nil {
		response.InternalError(c, "usage lookup not configured")
		return
	}
	u, err := lookup(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if u == nil {
		response.ErrorWithDetails(c, http.StatusNotFound, "usage record not found", "USAGE_NOT_FOUND", nil)
		return
	}
	cands, err := s.Resolve(u)
	if err != nil {
		response.InternalError(c, "archive index read failed: "+err.Error())
		return
	}
	result := gin.H{"usage": u, "candidates": cands}
	chosen := strings.TrimSpace(c.Query("ref"))
	switch {
	case chosen != "":
		found := false
		for _, cand := range cands {
			found = found || cand.Ref == chosen
		}
		if !found {
			response.ErrorWithDetails(c, http.StatusBadRequest, "ref does not belong to this usage record", "AUDIT_REF_MISMATCH", nil)
			return
		}
		result["resolution"] = "chosen"
	case len(cands) == 0:
		result["resolution"] = "not_found"
		response.Success(c, result)
		return
	case len(cands) == 1:
		chosen = cands[0].Ref
		result["resolution"] = "unique"
	default:
		best, tie := -1, false
		for i, cand := range cands {
			if best < 0 || cand.Score > cands[best].Score {
				best, tie = i, false
			} else if cand.Score == cands[best].Score {
				tie = true
			}
		}
		if tie {
			result["resolution"] = "ambiguous"
			response.Success(c, result)
			return
		}
		chosen = cands[best].Ref
		result["resolution"] = "best_match_with_alternatives"
	}
	av, err := s.ReadArchive(chosen)
	if err != nil {
		response.InternalError(c, err.Error())
		return
	}
	result["ref"] = chosen
	result["archive"] = av
	response.Success(c, result)
}

func handleLookup(c *gin.Context) {
	s := storeOr503(c)
	if s == nil {
		return
	}
	id := strings.TrimSpace(c.Query("request_id"))
	if id == "" {
		response.BadRequest(c, "request_id is required")
		return
	}
	cands, err := s.Resolve(&UsageRef{RequestID: id})
	if err != nil {
		response.InternalError(c, err.Error())
		return
	}
	response.Success(c, gin.H{"candidates": cands})
}

func handleArchive(c *gin.Context) {
	s := storeOr503(c)
	if s == nil {
		return
	}
	ref := c.Query("ref")
	if !ValidRef(ref) {
		response.BadRequest(c, "invalid ref")
		return
	}
	av, err := s.ReadArchive(ref)
	if err != nil {
		response.InternalError(c, err.Error())
		return
	}
	response.Success(c, av)
}

// handleBody 下载某段正文：decode=0 为原始字节（与 SHA256 头一致），
// decode=1 按 Content-Encoding 在内存中流式解码。传输压缩交给前置 Nginx 协商。
func handleBody(c *gin.Context) {
	s := storeOr503(c)
	if s == nil {
		return
	}
	ref, kind := c.Query("ref"), c.Query("kind")
	index, err := strconv.Atoi(c.DefaultQuery("index", "0"))
	if err != nil || index < 0 {
		response.BadRequest(c, "invalid index")
		return
	}
	if _, err := s.FilePath(ref, kind); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	decode := c.Query("decode") == "1"
	started := false
	_, err = s.StreamBody(c.Writer, ref, kind, index, decode, func(info BodyInfo) {
		started = true
		h := info.Header
		name := fmt.Sprintf("%s-%s-%d.bin", path.Base(ref), kind, index)
		c.Header("Content-Type", "application/octet-stream")
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Cache-Control", "private, no-store")
		if ct := headerValue(h.Header, "Content-Type"); ct != "" {
			c.Header("X-Archive-Original-Content-Type", ct)
		}
		if enc := headerValue(h.Header, "Content-Encoding"); enc != "" {
			c.Header("X-Archive-Content-Encoding", enc)
		}
		if !decode {
			c.Header("X-Archive-Body-SHA256", info.SHA256)
			c.Header("X-Archive-Body-Bytes", strconv.FormatInt(info.Bytes, 10))
			c.Header("X-Archive-Intact", strconv.FormatBool(info.Intact))
		}
		if info.End != nil {
			c.Header("X-Archive-Complete", strconv.FormatBool(info.End.Complete))
		}
		c.Status(http.StatusOK)
	})
	if err != nil && !started {
		if errors.Is(err, errSectionNotFound) {
			response.NotFound(c, err.Error())
			return
		}
		response.NotFound(c, "archive body unavailable: "+err.Error())
	}
}

var errSectionNotFound = errors.New("audit: section not found")
