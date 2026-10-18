package audit

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var (
	defaultStore atomic.Pointer[Store]
	defaultOnce  sync.Once
)

// Init 按 viper 配置（audit.* / AUDIT_*）初始化进程级归档存储，只执行一次。
// 由服务启动流程在配置加载后调用；未调用 Init 的进程（测试、工具）不会归档。
func Init() *Store {
	defaultOnce.Do(func() {
		cfg := ConfigFromViper()
		if !cfg.Enabled {
			slog.Info("audit_archive_disabled")
			return
		}
		s, err := NewStore(cfg)
		if err != nil {
			slog.Error("audit_archive_init_failed", "error", err)
			return
		}
		slog.Info("audit_archive_enabled", "root", s.Root())
		defaultStore.Store(s)
	})
	return defaultStore.Load()
}

// Default 返回进程级归档存储；未初始化、被禁用或初始化失败时返回 nil，所有钩子随之透传。
func Default() *Store {
	return defaultStore.Load()
}

// SetDefault 替换进程级归档存储（测试与自定义装配使用）。
func SetDefault(s *Store) {
	defaultOnce.Do(func() {})
	defaultStore.Store(s)
}

// Shutdown 等待进程级归档完成落盘。
func Shutdown(ctx context.Context) error {
	return defaultStore.Load().Close(ctx)
}

// IdentityFunc 在请求结束时从 Gin 上下文提取关联字段（API Key、用户、分组、账户等）。
// 由装配层提供，以免 audit 包依赖业务包。
type IdentityFunc func(c *gin.Context) map[string]any

// WrapHandler 把归档采集包裹在 next（通常是 ClientRequestID 中间件）外层：
// 先预置 client_request_id 与归档器到 context，再执行 next（其内部 c.Next() 会跑完
// 整条业务链），返回后记录客户端响应并收尾。
func WrapHandler(next gin.HandlerFunc, ident IdentityFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		s := Default()
		if s == nil || c.Request == nil {
			next(c)
			return
		}
		s.serve(c, next, ident)
	}
}

func (s *Store) serve(c *gin.Context, next gin.HandlerFunc, ident IdentityFunc) {
	req := c.Request
	ctx := req.Context()
	reqID, _ := ctx.Value(ctxkey.ClientRequestID).(string)
	reqID = strings.TrimSpace(reqID)
	if reqID == "" {
		reqID = uuid.New().String()
		ctx = context.WithValue(ctx, ctxkey.ClientRequestID, reqID)
	}
	session, sessionHeader := SessionFromHeader(req.Header, s.cfg.SessionHeaders)
	now := s.now()
	rec, err := s.Begin(reqID, session, sessionHeader, now)
	if err != nil {
		s.stats.BeginErrors.Add(1)
		slog.Warn("audit_archive_begin_failed", "request_id", reqID, "error", err)
		c.Request = req.WithContext(ctx)
		next(c)
		return
	}
	ti := traceFromRequest(ctx, req.Header)
	rec.TraceID, rec.SpanID = ti.TraceID, ti.SpanID
	ctx = WithRecorder(ctx, rec)
	c.Request = req.WithContext(ctx)

	transport := "http"
	if strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		transport = "websocket"
	}
	reqSec := rec.startSection(KindClientRequest, SectionHeader{
		Time:       now,
		Method:     req.Method,
		URL:        s.red.URL(req.URL),
		Proto:      req.Proto,
		Host:       req.Host,
		RemoteAddr: req.RemoteAddr,
		Header:     s.red.Header(req.Header),
		Transport:  transport,
		Meta: map[string]any{
			"request_id":     reqID,
			"session_id":     session,
			"session_header": sessionHeader,
			"trace":          ti,
		},
	})
	var body *teeBody
	if req.Body != nil && req.Body != http.NoBody {
		body = &teeBody{ReadCloser: req.Body, sec: reqSec}
		c.Request.Body = body
	}
	w := &respWriter{ResponseWriter: c.Writer, rec: rec, red: s.red, reqSec: reqSec}
	c.Writer = w

	defer func() {
		panicked := recover()
		if c.Writer == w {
			c.Writer = w.ResponseWriter
		}
		s.finishRequest(c, rec, w, body, reqSec, ident, panicked)
		if panicked != nil {
			panic(panicked)
		}
	}()
	next(c)
}

func (s *Store) finishRequest(c *gin.Context, rec *Recorder, w *respWriter, body *teeBody, reqSec *section, ident IdentityFunc, panicked any) {
	meta := map[string]any{}
	if ident != nil {
		func() {
			defer func() { _ = recover() }()
			for k, v := range ident(c) {
				meta[k] = v
			}
		}()
	}
	if id, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string); id != "" && id != rec.ID {
		meta["client_request_id_final"] = id
	}
	meta["attempts"] = rec.Attempts()
	meta["duration_ms"] = time.Since(rec.Started).Milliseconds()
	if len(c.Errors) > 0 {
		meta["gin_errors"] = c.Errors.String()
	}
	if panicked != nil {
		meta["panic"] = true
	}

	// 客户端请求
	reqComplete, reqErr := true, ""
	if body != nil {
		reqComplete, reqErr = body.state()
	}
	reqSec.end(reqComplete, reqErr, nil)

	// 客户端响应
	w.ensureHeader()
	complete := panicked == nil && c.Request.Context().Err() == nil
	errText := ""
	if !complete {
		if panicked != nil {
			errText = "handler panic"
		} else {
			errText = "client context: " + c.Request.Context().Err().Error()
		}
	}
	if w.hijacked.Load() {
		meta["hijacked"] = true
	}
	meta["status"] = w.Status()
	meta["response_size"] = w.Size()
	w.sec().end(complete, errText, meta)
	rec.Finish()
}

// teeBody 在业务读取请求体的同时复制到归档；不提前读取，不改变读取行为。
type teeBody struct {
	io.ReadCloser
	sec    *section
	mu     sync.Mutex
	eof    bool
	err    error
	closed bool
}

func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	if n > 0 {
		t.sec.body(p[:n])
	}
	if err != nil {
		t.mu.Lock()
		if errors.Is(err, io.EOF) {
			t.eof = true
		} else if t.err == nil {
			t.err = err
		}
		t.mu.Unlock()
	}
	return n, err
}

func (t *teeBody) Close() error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	return t.ReadCloser.Close()
}

func (t *teeBody) state() (bool, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.eof:
		return true, ""
	case t.err != nil:
		return false, "read error: " + t.err.Error()
	default:
		return false, "request body not fully consumed by handler"
	}
}

// respWriter 复制写给客户端的响应头与正文；WebSocket 劫持后复制原始连接字节。
type respWriter struct {
	gin.ResponseWriter
	rec      *Recorder
	red      *redactor
	reqSec   *section
	once     sync.Once
	mu       sync.Mutex
	section  *section
	hijacked atomic.Bool
}

func (w *respWriter) sec() *section {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.section
}

func (w *respWriter) snapshotHeader(status int, extra map[string]any) {
	w.once.Do(func() {
		h := w.Header()
		transport := "http"
		if strings.HasPrefix(strings.ToLower(h.Get("Content-Type")), "text/event-stream") {
			transport = "sse"
		}
		if status == http.StatusSwitchingProtocols || w.hijacked.Load() {
			transport = "websocket"
		}
		sec := w.rec.startSection(KindClientResponse, SectionHeader{
			Status:    status,
			Header:    w.red.Header(h),
			Transport: transport,
			Meta:      extra,
		})
		w.mu.Lock()
		w.section = sec
		w.mu.Unlock()
	})
}

func (w *respWriter) ensureHeader() {
	w.snapshotHeader(w.Status(), map[string]any{"header_snapshot": "at_finish"})
}

func (w *respWriter) WriteHeaderNow() {
	w.snapshotHeader(w.Status(), nil)
	w.ResponseWriter.WriteHeaderNow()
}

func (w *respWriter) Write(p []byte) (int, error) {
	w.snapshotHeader(w.Status(), nil)
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		w.sec().body(p[:n])
	}
	return n, err
}

func (w *respWriter) WriteString(str string) (int, error) {
	w.snapshotHeader(w.Status(), nil)
	n, err := w.ResponseWriter.WriteString(str)
	if n > 0 {
		w.sec().body([]byte(str[:n]))
	}
	return n, err
}

// Unwrap 支持 http.ResponseController。
func (w *respWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack 劫持后：客户端→服务端的原始字节追加到 client_request，
// 服务端→客户端的原始字节追加到 client_response（FrameRaw），展示时再解析 WS 帧。
func (w *respWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := w.ResponseWriter.Hijack()
	if err != nil {
		return conn, brw, err
	}
	w.hijacked.Store(true)
	w.snapshotHeader(w.Status(), map[string]any{"hijacked": true})
	tc := &teeConn{Conn: conn, in: w.reqSec, out: w.sec()}
	if brw == nil {
		return tc, brw, nil
	}
	// 劫持前已被 http.Server 缓冲的客户端字节同样属于入站流。
	if n := brw.Reader.Buffered(); n > 0 {
		if b, perr := brw.Peek(n); perr == nil {
			tc.in.raw(b)
		}
	}
	// 原 Writer 直接写底层连接，替换为写入 teeConn 的新 Writer；先冲刷遗留数据。
	_ = brw.Flush()
	return tc, bufio.NewReadWriter(brw.Reader, bufio.NewWriterSize(tc, brw.Writer.Size())), nil
}

type teeConn struct {
	net.Conn
	in  *section
	out *section
}

func (t *teeConn) Read(p []byte) (int, error) {
	n, err := t.Conn.Read(p)
	if n > 0 {
		t.in.raw(p[:n])
	}
	return n, err
}

func (t *teeConn) Write(p []byte) (int, error) {
	n, err := t.Conn.Write(p)
	if n > 0 {
		t.out.raw(p[:n])
	}
	return n, err
}
