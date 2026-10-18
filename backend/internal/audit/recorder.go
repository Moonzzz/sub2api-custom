package audit

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cespare/xxhash/v2"
)

func xxh64Hex(v uint64) string {
	s := strconv.FormatUint(v, 16)
	for len(s) < 16 {
		s = "0" + s
	}
	return s
}

type ctxKey struct{}

// WithRecorder 把请求归档器放入 context，供 RoundTripper / WS Tap / OnUsage 查找。
func WithRecorder(ctx context.Context, r *Recorder) context.Context {
	return context.WithValue(ctx, ctxKey{}, r)
}

// FromContext 取出请求归档器；未启用归档时返回 nil。
func FromContext(ctx context.Context) *Recorder {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(ctxKey{}).(*Recorder)
	return r
}

// Recorder 是单个客户端请求的归档器（四个阶段写入同一个文件）。
type Recorder struct {
	store         *Store
	ID            string
	Session       string
	SessionHeader string
	TraceID       string
	sessDir       string
	SpanID        string
	Started       time.Time

	file     *archiveFile
	attempts atomic.Int32
	finished atomic.Bool

	mu     sync.Mutex
	open   map[*section]struct{}
	lastWS int32
	ref    string
	usage  []IndexEntry // 结束前收到的用量回调，落盘时写入索引

	fileOnce sync.Once
	fileRes  *os.File
	fileErr  error
	unlock   func() // 会话写入锁，文件关闭后释放
}

func (r *Recorder) releaseLock() {
	if r.unlock != nil {
		r.unlock()
		r.unlock = nil
	}
}

// ArchiveRef 返回归档位置（相对根目录）；文件尚未创建时为空。
func (r *Recorder) ArchiveRef() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ref
}

// NextAttempt 分配一个上游调度尝试序号（从 1 开始）。
func (r *Recorder) NextAttempt() int {
	return int(r.attempts.Add(1))
}

// Attempts 返回已分配的尝试数。
func (r *Recorder) Attempts() int { return int(r.attempts.Load()) }

// section 是文件内一段报文的写入句柄，负责字节计数与 xxhash64。
type section struct {
	rec      *Recorder
	file     *archiveFile
	kind     byte
	seq      uint16
	mu       sync.Mutex
	h        *xxhash.Digest
	n        int64
	rawIn    int64
	messages int64
	ended    bool
}

func (r *Recorder) startSection(kind string, hdr SectionHeader) *section {
	if r == nil || r.finished.Load() {
		return nil
	}
	ki := kindIndex(kind)
	if ki < 0 {
		return nil
	}
	f := r.file
	hdr.Kind = kind
	if hdr.Time.IsZero() {
		hdr.Time = time.Now()
	}
	payload, err := json.Marshal(hdr)
	if err != nil {
		return nil
	}
	seq := uint16(hdr.Seq) //nolint:gosec // attempts are bounded far below 65535
	if err := f.writeFrame(FrameHeader, byte(ki), seq, payload); err != nil {
		r.store.stats.DroppedWrites.Add(1)
		return nil
	}
	s := &section{rec: r, file: f, kind: byte(ki), seq: seq, h: xxhash.New()}
	r.mu.Lock()
	r.open[s] = struct{}{}
	r.mu.Unlock()
	return s
}

func (s *section) body(p []byte) {
	if s == nil || len(p) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	if err := s.file.writeFrame(FrameBody, s.kind, s.seq, p); err != nil {
		s.rec.store.stats.DroppedWrites.Add(1)
		return
	}
	_, _ = s.h.Write(p)
	s.n += int64(len(p))
}

// raw 记录协议升级后的原始连接字节（不计入正文哈希）。
func (s *section) raw(p []byte) {
	if s == nil || len(p) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	if err := s.file.writeFrame(FrameRaw, s.kind, s.seq, p); err != nil {
		s.rec.store.stats.DroppedWrites.Add(1)
		return
	}
	s.rawIn += int64(len(p))
}

// message 记录一条 WebSocket 消息。
func (s *section) message(op byte, data []byte) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	buf := make([]byte, 9+len(data))
	buf[0] = op
	binary.BigEndian.PutUint64(buf[1:9], uint64(time.Now().UnixNano())) //nolint:gosec // 时间戳为正
	copy(buf[9:], data)
	if err := s.file.writeFrame(FrameMsg, s.kind, s.seq, buf); err != nil {
		s.rec.store.stats.DroppedWrites.Add(1)
		return
	}
	s.messages++
}

func (s *section) end(complete bool, errText string, meta map[string]any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	e := SectionEnd{
		Time:     time.Now(),
		Bytes:    s.n,
		XXH64:    xxh64Hex(s.h.Sum64()),
		Complete: complete,
		Error:    errText,
		Meta:     meta,
		RawIn:    s.rawIn,
		Messages: s.messages,
	}
	payload, _ := json.Marshal(e)
	if err := s.file.writeFrame(FrameEnd, s.kind, s.seq, payload); err != nil {
		s.rec.store.stats.DroppedWrites.Add(1)
	}
	s.mu.Unlock()

	s.rec.mu.Lock()
	delete(s.rec.open, s)
	s.rec.mu.Unlock()
}

// Note 追加一条附注到客户端响应文件（例如 WS 连接复用、用量回调）。
func (r *Recorder) Note(kind string, seq int, note map[string]any) {
	if r == nil || r.finished.Load() {
		return
	}
	ki := kindIndex(kind)
	if ki < 0 {
		return
	}
	note = maps.Clone(note)
	if note == nil {
		note = map[string]any{}
	}
	note["time"] = time.Now()
	payload, err := json.Marshal(note)
	if err != nil {
		return
	}
	if r.file.writeFrame(FrameNote, byte(ki), uint16(seq), payload) != nil { //nolint:gosec // bounded
		r.store.stats.DroppedWrites.Add(1)
	}
}

// Finish 结束归档：未结束的 section 标记为不完整，随后在后台创建文件、
// 完成压缩落盘并写索引，不阻塞客户端响应的收尾。
func (r *Recorder) Finish() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.finished.Load() {
		r.mu.Unlock()
		return
	}
	r.finished.Store(true)
	open := make([]*section, 0, len(r.open))
	for s := range r.open {
		open = append(open, s)
	}
	usage := r.usage
	r.usage = nil
	r.mu.Unlock()
	for _, s := range open {
		s.end(false, "archive finalized before stream closed", nil)
	}
	r.store.stats.FlushBacklog.Add(1)
	go func() {
		defer r.store.pending.Done()
		// 有界并发：限制同时创建/写入的文件数，突发流量下排队而不是耗尽文件句柄。
		r.store.flushSem <- struct{}{}
		r.store.stats.FlushBacklog.Add(-1)
		defer func() { <-r.store.flushSem }()
		if _, err := r.ensureFile(); err == nil {
			ref := r.ArchiveRef()
			for _, e := range usage {
				e.Ref = ref
				r.store.writeUsageIndex(e)
			}
		}
		raw := r.file.rawSize()
		if err := r.file.close(); err != nil {
			r.store.stats.CloseErrors.Add(1)
			slog.Warn("audit_archive_close_failed", "request_id", r.ID, "error", err)
		}
		// 文件已完整落盘（或创建失败），释放会话写入锁，夜间归档方可处理该会话。
		r.releaseLock()
		r.store.stats.RawBytes.Add(raw)
	}()
}

// Finished 表示归档已结束（之后的写入将被丢弃并计数）。
func (r *Recorder) Finished() bool { return r == nil || r.finished.Load() }
