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
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Store 管理归档根目录、会话目录映射、请求索引与压缩器。
type Store struct {
	cfg      Config
	root     string
	red      *redactor
	sessions *sessionDirs
	codec    *codec
	pending  sync.WaitGroup
	flushSem chan struct{}
	closed   atomic.Bool
	now      func() time.Time

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

// NewStore 创建归档存储；根目录不存在时自动创建。
func NewStore(cfg Config) (*Store, error) {
	if strings.TrimSpace(cfg.RootDir) == "" {
		cfg.RootDir = DefaultConfig().RootDir
	}
	root, err := filepath.Abs(cfg.RootDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("audit: create root dir: %w", err)
	}
	c, err := newCodec(cfg.EncoderLevel)
	if err != nil {
		return nil, err
	}
	if cfg.MemoryBufferBytes <= 0 {
		cfg.MemoryBufferBytes = DefaultConfig().MemoryBufferBytes
	}
	if len(cfg.SessionHeaders) == 0 {
		cfg.SessionHeaders = DefaultSessionHeaders
	}
	if cfg.FlushWorkers <= 0 {
		cfg.FlushWorkers = DefaultConfig().FlushWorkers
	}
	return &Store{
		cfg:      cfg,
		root:     root,
		red:      newRedactor(cfg),
		sessions: newSessionDirs(root, cfg.SessionCacheSize),
		codec:    c,
		flushSem: make(chan struct{}, cfg.FlushWorkers),
		now:      time.Now,
	}, nil
}

// Root 返回归档根目录的绝对路径。
func (s *Store) Root() string { return s.root }

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

// Close 停止接收新归档并等待已结束请求的压缩落盘完成。
func (s *Store) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.closed.Store(true)
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

// ValidRef 校验归档引用（相对根目录的基础路径，不含 kind 后缀）。
func ValidRef(ref string) bool {
	return refRE.MatchString(ref) && !strings.Contains(ref, "..")
}

// FilePath 返回某个归档引用下指定 kind 的文件绝对路径。
func (s *Store) FilePath(ref, kind string) (string, error) {
	if !ValidRef(ref) {
		return "", fmt.Errorf("audit: invalid archive ref")
	}
	valid := false
	for _, k := range Kinds {
		valid = valid || k == kind
	}
	if !valid {
		return "", fmt.Errorf("audit: invalid kind")
	}
	p := filepath.Join(s.root, filepath.FromSlash(ref)+"-"+kind+".zst")
	if !strings.HasPrefix(p, s.root+string(filepath.Separator)) {
		return "", fmt.Errorf("audit: invalid archive ref")
	}
	return p, nil
}

// Begin 为一个客户端请求创建归档器。
//
// 请求热路径上只解析会话目录（有界 LRU 命中为纯内存；未命中时读/建一次标记文件，
// 保证“最先开始的请求”固定会话日期）；目录与四个文件的创建、索引追加全部推迟到后台
// （请求结束后的落盘 goroutine，或大正文切换流式压缩时）。
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
	for i := range Kinds {
		idx := i
		rec.files[i] = newArchiveFile(func() (*os.File, error) {
			files, err := rec.ensureFiles()
			if err != nil {
				return nil, err
			}
			return files[idx], nil
		}, s.codec, s.cfg.MemoryBufferBytes)
	}
	s.pending.Add(1)
	s.stats.Requests.Add(1)
	return rec, nil
}

// ensureFiles 只执行一次：定位会话目录（固定首次日期），以 O_EXCL 创建四个文件
// （{yyyy-MM}/{dd}/{session}/{requestId}-{n}-{kind}.zst，n 递增，重复请求 ID 不覆盖），
// 并追加请求索引。
func (r *Recorder) ensureFiles() ([4]*os.File, error) {
	r.filesOnce.Do(func() {
		r.filesRes, r.filesErr = r.store.createFiles(r)
		if r.filesErr != nil {
			r.store.stats.BeginErrors.Add(1)
			slog.Warn("audit_archive_create_failed", "request_id", r.ID, "error", r.filesErr)
		}
	})
	return r.filesRes, r.filesErr
}

func (s *Store) createFiles(r *Recorder) ([4]*os.File, error) {
	var files [4]*os.File
	sessDir := r.sessDir
	absDir := filepath.Join(s.root, filepath.FromSlash(sessDir))
	if err := os.MkdirAll(absDir, 0o750); err != nil {
		return files, err
	}
	name := safeName(r.ID)
	for n := 1; n <= 999999; n++ {
		base := fmt.Sprintf("%s-%d", name, n)
		f, err := os.OpenFile(filepath.Join(absDir, base+"-"+KindClientRequest+".zst"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return files, err
		}
		files[0] = f
		for i, kind := range Kinds[1:] {
			f, err := os.OpenFile(filepath.Join(absDir, base+"-"+kind+".zst"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
			if err != nil {
				for _, opened := range files[:i+1] {
					_ = opened.Close()
				}
				return [4]*os.File{}, err
			}
			files[i+1] = f
		}
		ref := sessDir + "/" + base
		r.mu.Lock()
		r.ref = ref
		r.mu.Unlock()
		s.appendIndex(r.ID, IndexEntry{Ref: ref, Time: r.Started, Via: "client_request_id", RequestID: r.ID, Session: r.Session})
		return files, nil
	}
	return files, errors.New("audit: too many duplicate request ids")
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
	return filepath.Join(s.root, ".index", h[:2], h[2:4], h+".idx")
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
