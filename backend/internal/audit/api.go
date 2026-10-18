package audit

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	a.GET("/bundle", handleBundle)
	a.POST("/archive/run", handleRunArchive)
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
	response.Success(c, gin.H{
		"enabled":            true,
		"root":               s.Root(),
		"active_dir":         s.ActiveDir(),
		"archive_dir":        s.ArchiveDir(),
		"archive_after":      s.cfg.ArchiveAfter.String(),
		"archive_at":         s.cfg.ArchiveAt,
		"archive_running":    s.ArchiveRunning(),
		"last_archive":       s.LastArchiveReport(),
		"stats":              s.StatsSnapshot(),
		"display_body_limit": s.cfg.DisplayBodyLimit,
	})
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
	if !ValidRef(ref) || !ValidKind(kind) {
		response.BadRequest(c, "invalid ref or kind")
		return
	}
	decode := c.Query("decode") == "1"
	started := false
	_, err = s.StreamBody(c.Writer, ref, kind, index, decode, func(info BodyInfo) {
		started = true
		h := info.Header
		suffix := ""
		if decode {
			suffix = "-decoded"
		}
		name := fmt.Sprintf("%s-%s-%d%s%s", path.Base(ref), kind, index, suffix, bodyFileExt(h.Header, decode))
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

// bodyFileExt 按正文类型给下载文件后缀：保留原始压缩字节时按 Content-Encoding；
// 否则 JSON 与 SSE 为 .json，其它文本为 .txt，识别不了的为 .bin。
func bodyFileExt(h map[string][]string, decoded bool) string {
	enc := strings.ToLower(strings.TrimSpace(headerValue(h, "Content-Encoding")))
	if !decoded && enc != "" && enc != "identity" {
		switch enc {
		case "gzip", "x-gzip":
			return ".gz"
		case "br":
			return ".br"
		case "zstd":
			return ".zst"
		case "deflate":
			return ".deflate"
		}
		return ".bin"
	}
	ct := strings.ToLower(headerValue(h, "Content-Type"))
	switch {
	case strings.Contains(ct, "json"), strings.HasPrefix(ct, "text/event-stream"):
		return ".json"
	case strings.HasPrefix(ct, "text/"):
		return ".txt"
	}
	return ".bin"
}

// handleBundle 下载整个请求：zip 内只有一个文件 {请求}.zst，即该请求的存档数据原样
// （四个阶段共用一条 zstd 流，格式同 active 请求文件；已归档的请求从会话归档中还原为同格式）。
// zip 条目使用 Store（.zst 已压缩，不再二次压缩）。
func handleBundle(c *gin.Context) {
	s := storeOr503(c)
	if s == nil {
		return
	}
	ref := c.Query("ref")
	if !ValidRef(ref) {
		response.BadRequest(c, "invalid ref")
		return
	}
	base := path.Base(ref)
	var zw *zip.Writer
	entry := &lazyHeaderWriter{onFirst: func() (io.Writer, error) {
		c.Header("Content-Type", "application/zip")
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", base+".zip"))
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Cache-Control", "private, no-store")
		c.Status(http.StatusOK)
		zw = zip.NewWriter(c.Writer)
		return zw.CreateHeader(&zip.FileHeader{Name: base + ".zst", Method: zip.Store, Modified: time.Now()})
	}}
	_, err := s.WriteRequestBundle(entry, ref)
	if !entry.started {
		if errors.Is(err, errRequestNotFound) {
			response.NotFound(c, "archive not found")
			return
		}
		msg := "empty archive"
		if err != nil {
			msg = err.Error()
		}
		response.InternalError(c, "archive read failed: "+msg)
		return
	}
	if err != nil {
		// 响应头已发出：不写 zip 目录，客户端得到的是无法解开的不完整文件，而不是“看似完整”的文件。
		slog.Warn("audit_bundle_stream_failed", "ref", ref, "error", err)
		return
	}
	_ = zw.Close()
}

// lazyHeaderWriter 在第一次写入时才发送响应头并取得真正的写入目标，
// 便于读取失败（例如请求不存在）时仍能返回 JSON 错误。
type lazyHeaderWriter struct {
	w       io.Writer
	onFirst func() (io.Writer, error)
	started bool
}

func (l *lazyHeaderWriter) Write(p []byte) (int, error) {
	if !l.started {
		l.started = true
		w, err := l.onFirst()
		if err != nil {
			return 0, err
		}
		l.w = w
	}
	if l.w == nil {
		return 0, io.ErrClosedPipe
	}
	return l.w.Write(p)
}

// handleRunArchive 立即执行一次夜间归档（同步返回结果；已有归档在执行时返回 409）。
func handleRunArchive(c *gin.Context) {
	s := storeOr503(c)
	if s == nil {
		return
	}
	rep, err := s.RunArchive(c.Request.Context())
	if errors.Is(err, errArchiveRunning) {
		response.ErrorWithDetails(c, http.StatusConflict, "archive already running", "AUDIT_ARCHIVE_RUNNING", nil)
		return
	}
	if err != nil {
		response.InternalError(c, err.Error())
		return
	}
	response.Success(c, rep)
}

var errSectionNotFound = errors.New("audit: section not found")
