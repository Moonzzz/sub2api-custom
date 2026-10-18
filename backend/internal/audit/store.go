package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Store 是请求报文日志的存取组件：写入（active）、查询（active → archive）、
// 夜间归档（active → archive）都经由它完成，业务侧只通过钩子接入。
type Store struct {
	cfg        Config
	activeDir  string
	archiveDir string
	red        *redactor
	sessions   *sessionDirs
	locks      *sessionLocks
	codec      *codec
	pending    sync.WaitGroup
	flushSem   chan struct{}
	// archiveReads 限制同时解压会话归档的数量。
	archiveReads chan struct{}
	closed       atomic.Bool
	now          func() time.Time

	archiver *archiver

	// 统计（用于性能报告与健康检查）
	stats Stats
}

// Stats 是归档写入的累计计数。
type Stats struct {
	Requests      atomic.Int64
	BeginErrors   atomic.Int64
	CloseErrors   atomic.Int64
	RawBytes      atomic.Int64
	DroppedWrites atomic.Int64
	FlushBacklog  atomic.Int64
}

// NewStore 创建存取组件；目录不存在时自动创建。定时归档需调用 Start 启动。
func NewStore(cfg Config) (*Store, error) {
	def := DefaultConfig()
	if strings.TrimSpace(cfg.RootDir) == "" {
		cfg.RootDir = def.RootDir
	}
	if strings.TrimSpace(cfg.ActiveDir) == "" {
		cfg.ActiveDir = filepath.Join(cfg.RootDir, "active")
	}
	if strings.TrimSpace(cfg.ArchiveDir) == "" {
		cfg.ArchiveDir = filepath.Join(cfg.RootDir, "archive")
	}
	active, err := filepath.Abs(cfg.ActiveDir)
	if err != nil {
		return nil, err
	}
	archive, err := filepath.Abs(cfg.ArchiveDir)
	if err != nil {
		return nil, err
	}
	if active == archive || strings.HasPrefix(archive, active+string(filepath.Separator)) || strings.HasPrefix(active, archive+string(filepath.Separator)) {
		return nil, fmt.Errorf("audit: active_dir and archive_dir must not overlap")
	}
	for _, d := range []string{active, archive} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("audit: create dir %s: %w", d, err)
		}
	}
	if cfg.ActiveWindowMB <= 0 {
		cfg.ActiveWindowMB = def.ActiveWindowMB
	}
	if cfg.ArchiveWindowMB <= 0 {
		cfg.ArchiveWindowMB = def.ArchiveWindowMB
	}
	if cfg.ArchiveAfter <= 0 {
		cfg.ArchiveAfter = def.ArchiveAfter
	}
	if strings.TrimSpace(cfg.ArchiveEncoderLevel) == "" {
		cfg.ArchiveEncoderLevel = def.ArchiveEncoderLevel
	}
	c, err := newCodec(cfg.EncoderLevel, cfg.ActiveWindowMB)
	if err != nil {
		return nil, err
	}
	if cfg.MemoryBufferBytes <= 0 {
		cfg.MemoryBufferBytes = def.MemoryBufferBytes
	}
	if len(cfg.SessionHeaders) == 0 {
		cfg.SessionHeaders = DefaultSessionHeaders
	}
	if cfg.FlushWorkers <= 0 {
		cfg.FlushWorkers = def.FlushWorkers
	}
	s := &Store{
		cfg:          cfg,
		activeDir:    active,
		archiveDir:   archive,
		red:          newRedactor(cfg),
		sessions:     newSessionDirs(active, cfg.SessionCacheSize),
		locks:        newSessionLocks(),
		codec:        c,
		flushSem:     make(chan struct{}, cfg.FlushWorkers),
		archiveReads: make(chan struct{}, archiveReadSlots),
		now:          time.Now,
	}
	s.archiver = newArchiver(s)
	return s, nil
}

// Root 返回 active 目录的绝对路径（新请求写入位置）。
func (s *Store) Root() string { return s.activeDir }

// ActiveDir 返回 active 目录的绝对路径。
func (s *Store) ActiveDir() string { return s.activeDir }

// ArchiveDir 返回 archive 目录的绝对路径。
func (s *Store) ArchiveDir() string { return s.archiveDir }

// Config 返回生效配置。
func (s *Store) Config() Config { return s.cfg }

// StatsSnapshot 返回统计快照。
func (s *Store) StatsSnapshot() map[string]int64 {
	return map[string]int64{
		"requests":       s.stats.Requests.Load(),
		"begin_errors":   s.stats.BeginErrors.Load(),
		"close_errors":   s.stats.CloseErrors.Load(),
		"raw_bytes":      s.stats.RawBytes.Load(),
		"dropped_writes": s.stats.DroppedWrites.Load(),
		"flush_backlog":  s.stats.FlushBacklog.Load(),
		"session_cache":  int64(s.sessions.len()),
	}
}

// Close 停止接收新归档、停止定时归档，并等待已结束请求的压缩落盘完成。
func (s *Store) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.closed.Store(true)
	s.archiver.stop()
	done := make(chan struct{})
	go func() {
		s.pending.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var refRE = regexp.MustCompile(`^\d{4}-\d{2}/\d{2}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}-\d{1,6}$`)

// ValidRef 校验归档引用：{yyyy-MM}/{dd}/{session}/{requestId}-{n}。
func ValidRef(ref string) bool {
	return refRE.MatchString(ref) && !strings.Contains(ref, "..")
}

// refSessionDir 返回引用所属的会话目录（{yyyy-MM}/{dd}/{session}）。
func refSessionDir(ref string) string { return path.Dir(ref) }

// activeRequestPath 返回请求在 active 中的文件路径。
func (s *Store) activeRequestPath(ref string) string {
	return filepath.Join(s.activeDir, filepath.FromSlash(ref)+".zst")
}

// archiveSessionPath 返回会话在 archive 中的文件路径。
func (s *Store) archiveSessionPath(sessDir string) string {
	return filepath.Join(s.archiveDir, filepath.FromSlash(sessDir)+".zst")
}

// Begin 为一个客户端请求创建归档器。
//
// 请求热路径上只解析会话目录（有界 LRU 命中为纯内存；未命中时读/建一次标记文件，
// 保证“最先开始的请求”固定会话日期）；目录与文件的创建、索引追加全部推迟到后台
// （请求结束后的落盘 goroutine，或大正文切换流式压缩时）。
// 会话即使已被归档，新请求也照常写入 active 中同一相对路径，由夜间任务合并。
func (s *Store) Begin(requestID, session, sessionHeader string, now time.Time) (*Recorder, error) {
	if s.closed.Load() {
		return nil, errors.New("audit: store closed")
	}
	sessDir, err := s.sessions.Resolve(session, now)
	if err != nil {
		return nil, err
	}
	rec := &Recorder{
		sessDir:       sessDir,
		store:         s,
		ID:            requestID,
		Session:       session,
		SessionHeader: sessionHeader,
		Started:       now,
		open:          make(map[*section]struct{}),
	}
	rec.file = newArchiveFile(rec.ensureFile, s.codec, s.cfg.MemoryBufferBytes)
	s.pending.Add(1)
	s.stats.Requests.Add(1)
	return rec, nil
}

// ensureFile 只执行一次：获取会话写入锁，以 O_EXCL 创建
// {active}/{yyyy-MM}/{dd}/{session}/{requestId}-{n}.zst（n 递增；重复请求 ID 不覆盖，
// 也不与已归档的同名请求冲突），并追加请求索引。
func (r *Recorder) ensureFile() (*os.File, error) {
	r.fileOnce.Do(func() {
		r.unlock = r.store.locks.RLock(r.sessDir)
		r.fileRes, r.fileErr = r.store.createFile(r)
		if r.fileErr != nil {
			r.releaseLock()
			r.store.stats.BeginErrors.Add(1)
			slog.Warn("audit_archive_create_failed", "request_id", r.ID, "error", r.fileErr)
		}
	})
	return r.fileRes, r.fileErr
}

func (s *Store) createFile(r *Recorder) (*os.File, error) {
	sessDir := r.sessDir
	absDir := filepath.Join(s.activeDir, filepath.FromSlash(sessDir))
	if err := os.MkdirAll(absDir, 0o750); err != nil {
		return nil, err
	}
	// 已归档的同名引用（会话重激活后出现重复请求 ID）由索引给出，O(1) 读取。
	taken := map[string]bool{}
	if entries, err := s.Lookup(r.ID); err == nil {
		for _, e := range entries {
			taken[e.Ref] = true
		}
	}
	name := safeName(r.ID)
	for n := 1; n <= 999999; n++ {
		base := fmt.Sprintf("%s-%d", name, n)
		ref := sessDir + "/" + base
		if taken[ref] {
			continue
		}
		f, err := os.OpenFile(filepath.Join(absDir, base+".zst"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		r.ref = ref
		r.mu.Unlock()
		s.appendIndex(r.ID, IndexEntry{Ref: ref, Time: r.Started, Via: "client_request_id", RequestID: r.ID, Session: r.Session})
		return f, nil
	}
	return nil, errors.New("audit: too many duplicate request ids")
}

// IndexEntry 是请求索引中的一行。索引按 ID 的 sha256 分桶，查找为 O(1) 文件读取。
type IndexEntry struct {
	Ref            string    `json:"ref"`
	Time           time.Time `json:"t"`
	Via            string    `json:"via"`
	RequestID      string    `json:"request_id,omitempty"`
	Session        string    `json:"session,omitempty"`
	UsageRequestID string    `json:"usage_request_id,omitempty"`
	UsageState     string    `json:"usage_state,omitempty"`
	UsageError     string    `json:"usage_error,omitempty"`
	APIKeyID       int64     `json:"api_key_id,omitempty"`
	AccountID      int64     `json:"account_id,omitempty"`
	UserID         int64     `json:"user_id,omitempty"`
}

func (s *Store) indexPath(key string) string {
	h := hashKey(key)
	return filepath.Join(s.activeDir, ".index", h[:2], h[2:4], h+".idx")
}

func (s *Store) appendIndex(key string, e IndexEntry) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	p := s.indexPath(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		slog.Warn("audit_index_mkdir_failed", "error", err)
		return
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	line = append(line, '\n')
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640) //nolint:gosec // 路径由 sha256 派生
	if err != nil {
		slog.Warn("audit_index_open_failed", "error", err)
		return
	}
	// 单次 write + O_APPEND：多写者并发追加时行不会交错。
	if _, err := f.Write(line); err != nil {
		slog.Warn("audit_index_write_failed", "error", err)
	}
	_ = f.Close()
}

// Lookup 返回某个 ID（客户端请求 ID 或用量 request_id）对应的全部索引行。
func (s *Store) Lookup(key string) ([]IndexEntry, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, nil
	}
	f, err := os.Open(s.indexPath(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []IndexEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		var e IndexEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil && ValidRef(e.Ref) {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}
