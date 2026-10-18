package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
)

// 夜间归档：把 active 中不再活跃的会话合并进 archive 的单个会话文件。
//
//   - 只遍历 active（最多保留约 ArchiveAfter 时长的数据），不扫描 archive；
//     active 中的每个会话目录都是候选，会话目录本身就是“重激活记录”。
//   - 会话最后活跃时间 = 目录与其中文件的最大 mtime，请求路径上无需额外记录。
//   - 已有归档（会话被重激活过）时，流式解压旧归档并接上 active 中的新请求，
//     作为一条完整的 zstd 流重新压缩（保留跨请求去重），不在 active 落地解压。
//   - 写临时文件 → fsync → rename 覆盖 → fsync 目录 → 删除 active；
//     中途崩溃时以 active 为准重做，合并时按请求引用去重，不会重复写入。
//   - 会话正在写入（持有会话写入锁）即说明仍活跃，本轮跳过。

// ArchiveReport 是一次归档执行的结果。
type ArchiveReport struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Cutoff     time.Time `json:"cutoff"`
	// Sessions 本次归档的会话数，其中 Merged 个与已有归档合并（重激活会话）。
	Sessions int `json:"sessions"`
	Merged   int `json:"merged"`
	Requests int `json:"requests"`
	// Active 跳过的仍活跃会话数；Busy 跳过的正在写入的会话数。
	Active int `json:"active"`
	Busy   int `json:"busy"`
	Failed int `json:"failed"`
	// ActiveBytes 从 active 移出的压缩字节；ArchiveBytes 写出的会话文件总大小（含合并的旧内容）。
	ActiveBytes  int64    `json:"active_bytes"`
	ArchiveBytes int64    `json:"archive_bytes"`
	Errors       []string `json:"errors,omitempty"`
	Cancelled    bool     `json:"cancelled,omitempty"`
}

func (r *ArchiveReport) addError(format string, args ...any) {
	if len(r.Errors) < 50 {
		r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
	}
}

// sessionFrameLimit 单个 zstd frame 的原始数据上限；超过后另起一个 frame 继续写
// （同一文件内多个 frame 依旧是一条可顺序解压的流），只是兜底，正常会话远小于此值。
const sessionFrameLimit = 1 << 30

type archiver struct {
	s       *Store
	runMu   sync.Mutex // 同一时刻只执行一次归档
	running atomic.Bool
	last    atomic.Pointer[ArchiveReport]

	startOnce sync.Once
	stopOnce  sync.Once
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

func newArchiver(s *Store) *archiver {
	ctx, cancel := context.WithCancel(context.Background())
	return &archiver{s: s, ctx: ctx, cancel: cancel}
}

// Start 启动每日定时归档（ArchiveAt，默认 03:00；"off" 表示关闭）。可重复调用。
func (s *Store) Start() {
	a := s.archiver
	at := strings.TrimSpace(strings.ToLower(s.cfg.ArchiveAt))
	if at == "off" || at == "disabled" || at == "false" {
		slog.Info("audit_archive_schedule_disabled")
		return
	}
	hour, minute, err := parseClock(at)
	if err != nil {
		slog.Warn("audit_archive_schedule_invalid", "archive_at", s.cfg.ArchiveAt, "error", err)
		hour, minute = 3, 0
	}
	a.startOnce.Do(func() {
		a.wg.Add(1)
		go a.loop(hour, minute)
	})
}

func (a *archiver) loop(hour, minute int) {
	defer a.wg.Done()
	for {
		now := a.s.now()
		next := nextClock(now, hour, minute)
		t := time.NewTimer(next.Sub(now))
		select {
		case <-a.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		rep, err := a.s.RunArchive(a.ctx)
		if err != nil {
			slog.Warn("audit_archive_run_failed", "error", err)
			continue
		}
		slog.Info("audit_archive_run_done",
			"sessions", rep.Sessions, "merged", rep.Merged, "requests", rep.Requests,
			"active_skipped", rep.Active, "busy_skipped", rep.Busy, "failed", rep.Failed,
			"active_bytes", rep.ActiveBytes, "archive_bytes", rep.ArchiveBytes,
			"elapsed", rep.FinishedAt.Sub(rep.StartedAt).Round(time.Millisecond))
	}
}

func (a *archiver) stop() {
	a.stopOnce.Do(func() {
		a.cancel()
		a.wg.Wait()
	})
}

func parseClock(v string) (int, int, error) {
	t, err := time.Parse("15:04", v)
	if err != nil {
		return 0, 0, err
	}
	return t.Hour(), t.Minute(), nil
}

// nextClock 返回 now 之后（不含）最近一次的本地 hour:minute。
func nextClock(now time.Time, hour, minute int) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// LastArchiveReport 返回最近一次归档的结果（尚未执行时为 nil）。
func (s *Store) LastArchiveReport() *ArchiveReport { return s.archiver.last.Load() }

// ArchiveRunning 表示归档正在执行。
func (s *Store) ArchiveRunning() bool { return s.archiver.running.Load() }

// errArchiveRunning 表示已有归档在执行。
var errArchiveRunning = errors.New("audit: archive already running")

// RunArchive 立即执行一次归档：处理最后活跃时间早于 now-ArchiveAfter 的会话。
func (s *Store) RunArchive(ctx context.Context) (*ArchiveReport, error) {
	return s.ArchiveBefore(ctx, s.now().Add(-s.cfg.ArchiveAfter))
}

// ArchiveBefore 归档最后活跃时间早于 cutoff 的会话。
func (s *Store) ArchiveBefore(ctx context.Context, cutoff time.Time) (*ArchiveReport, error) {
	a := s.archiver
	if !a.runMu.TryLock() {
		return nil, errArchiveRunning
	}
	defer a.runMu.Unlock()
	a.running.Store(true)
	defer a.running.Store(false)

	rep := &ArchiveReport{StartedAt: s.now(), Cutoff: cutoff}
	dirs, err := s.listActiveSessions()
	if err != nil {
		return nil, err
	}
	for _, rel := range dirs {
		if ctx.Err() != nil {
			rep.Cancelled = true
			break
		}
		s.archiveSession(ctx, rel, cutoff, rep)
	}
	rep.FinishedAt = s.now()
	a.last.Store(rep)
	return rep, nil
}

var (
	monthDirRE    = regexp.MustCompile(`^\d{4}-\d{2}$`)
	dayDirRE      = regexp.MustCompile(`^\d{2}$`)
	requestFileRE = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]{0,95}-\d{1,6})\.zst$`)
)

// listActiveSessions 列出 active 中的会话目录（{yyyy-MM}/{dd}/{session}），按路径排序。
// 只遍历三层目录名，不读取文件。
func (s *Store) listActiveSessions() ([]string, error) {
	var out []string
	months, err := os.ReadDir(s.activeDir)
	if err != nil {
		return nil, err
	}
	for _, m := range months {
		if !m.IsDir() || !monthDirRE.MatchString(m.Name()) {
			continue
		}
		days, err := os.ReadDir(filepath.Join(s.activeDir, m.Name()))
		if err != nil {
			continue
		}
		for _, d := range days {
			if !d.IsDir() || !dayDirRE.MatchString(d.Name()) {
				continue
			}
			sessions, err := os.ReadDir(filepath.Join(s.activeDir, m.Name(), d.Name()))
			if err != nil {
				continue
			}
			for _, se := range sessions {
				if se.IsDir() && safeNameRE.MatchString(se.Name()) {
					out = append(out, m.Name()+"/"+d.Name()+"/"+se.Name())
				}
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

type activeFile struct {
	base  string // {requestId}-{n}
	path  string
	size  int64
	mtime time.Time
}

// archiveSession 归档单个会话；结果累加到 rep。
func (s *Store) archiveSession(ctx context.Context, rel string, cutoff time.Time, rep *ArchiveReport) {
	unlock := s.locks.TryLock(rel)
	if unlock == nil {
		rep.Busy++
		return
	}
	defer unlock()

	dir := filepath.Join(s.activeDir, filepath.FromSlash(rel))
	files, last, err := scanActiveSession(dir)
	if err != nil {
		rep.Failed++
		rep.addError("%s: scan: %v", rel, err)
		return
	}
	if last.After(cutoff) {
		rep.Active++
		return
	}
	if len(files) == 0 {
		removeEmptyDirs(s.activeDir, dir)
		return
	}
	merged, size, err := s.writeSessionArchive(ctx, rel, files)
	if err != nil {
		rep.Failed++
		rep.addError("%s: %v", rel, err)
		slog.Warn("audit_archive_session_failed", "session_dir", rel, "error", err)
		return
	}
	rep.Sessions++
	if merged {
		rep.Merged++
	}
	rep.Requests += len(files)
	rep.ArchiveBytes += size
	for _, f := range files {
		if err := os.Remove(f.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			rep.addError("%s: remove %s: %v", rel, f.base, err)
			continue
		}
		rep.ActiveBytes += f.size
	}
	removeEmptyDirs(s.activeDir, dir)
}

// scanActiveSession 列出会话目录中的请求文件，并返回最后活跃时间（目录与文件 mtime 的最大值）。
func scanActiveSession(dir string) ([]activeFile, time.Time, error) {
	st, err := os.Stat(dir)
	if err != nil {
		return nil, time.Time{}, err
	}
	last := st.ModTime()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, last, err
	}
	var files []activeFile
	for _, e := range entries {
		m := requestFileRE.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(last) {
			last = info.ModTime()
		}
		files = append(files, activeFile{base: m[1], path: filepath.Join(dir, e.Name()), size: info.Size(), mtime: info.ModTime()})
	}
	// 按完成时间排序：相邻请求的上下文重叠最多，紧挨着写入去重效果最好。
	sort.Slice(files, func(i, j int) bool {
		if !files[i].mtime.Equal(files[j].mtime) {
			return files[i].mtime.Before(files[j].mtime)
		}
		return files[i].base < files[j].base
	})
	return files, last, nil
}

// removeEmptyDirs 自下而上删除空目录，直到 root（不含）。
func removeEmptyDirs(root, dir string) {
	for dir != root && strings.HasPrefix(dir, root+string(filepath.Separator)) {
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// sessionWriter 把帧写入会话归档的压缩流，并在原始数据超过上限时另起 zstd frame。
type sessionWriter struct {
	enc   *zstd.Encoder
	inner io.Writer
	raw   int64
	hdr   []byte
}

func (w *sessionWriter) frame(typ, kind byte, seq uint16, payload []byte) error {
	if w.raw >= sessionFrameLimit {
		if err := w.enc.Close(); err != nil {
			return err
		}
		w.enc.Reset(w.inner)
		w.raw = 0
	}
	w.hdr = encodeFrameHeader(w.hdr[:0], typ, kind, seq, len(payload))
	if _, err := w.enc.Write(w.hdr); err != nil {
		return err
	}
	if _, err := w.enc.Write(payload); err != nil {
		return err
	}
	w.raw += int64(len(w.hdr) + len(payload))
	return nil
}

func (w *sessionWriter) json(typ byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.frame(typ, 0, 0, b)
}

// writeSessionArchive 生成会话归档：旧归档（若有）+ active 中的请求，写临时文件后原子替换。
// 返回是否合并了旧归档，以及新文件大小。
func (s *Store) writeSessionArchive(ctx context.Context, rel string, files []activeFile) (merged bool, size int64, err error) {
	dst := s.archiveSessionPath(rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return false, 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return false, 0, err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	bw := bufio.NewWriterSize(tmp, 1<<20)
	enc, err := zstd.NewWriter(bw, encoderOptions(s.cfg.ArchiveEncoderLevel, s.cfg.ArchiveWindowMB, false)...)
	if err != nil {
		return false, 0, err
	}
	w := &sessionWriter{enc: enc, inner: bw, hdr: make([]byte, 0, frameHeaderSize)}
	if _, err := enc.Write([]byte(sessionMagic)); err != nil {
		_ = enc.Close()
		return false, 0, err
	}

	// 1) 旧归档：逐帧复制，记录已归档的请求引用。旧归档损坏时放弃本会话（active 保持不动）。
	have := map[string]bool{}
	merged, err = s.copyOldArchive(dst, w, have)
	if err != nil {
		_ = enc.Close()
		return false, 0, fmt.Errorf("existing archive unreadable, session left in active: %w", err)
	}

	// 2) active 中的请求，按时间顺序追加。
	for _, f := range files {
		if ctx.Err() != nil {
			_ = enc.Close()
			return false, 0, ctx.Err()
		}
		ref := rel + "/" + f.base
		if have[ref] {
			continue // 上次归档已写入（rename 后、删除 active 前中断），只需删除 active
		}
		if err := w.json(FrameEntry, EntryHeader{Ref: ref, CompressedSize: f.size, ModTime: f.mtime}); err != nil {
			_ = enc.Close()
			return false, 0, err
		}
		var werr error
		info, rerr := s.walkActive(ref, func(fx frame) bool {
			werr = w.frame(fx.typ, fx.kind, fx.seq, fx.payload)
			return werr == nil
		})
		if werr != nil {
			_ = enc.Close()
			return false, 0, werr
		}
		end := EntryEnd{Status: info.Status}
		if rerr != nil {
			// 截断/损坏的请求文件：保留中断前的完整帧，并在条目尾记录状态。
			end.Error = rerr.Error()
			if errors.Is(rerr, errRequestNotFound) {
				end.Status, end.Error = "missing", ""
			}
		}
		if err := w.json(FrameEntryEnd, end); err != nil {
			_ = enc.Close()
			return false, 0, err
		}
	}

	if err := enc.Close(); err != nil {
		return false, 0, err
	}
	if err := bw.Flush(); err != nil {
		return false, 0, err
	}
	if err := tmp.Sync(); err != nil {
		return false, 0, err
	}
	st, err := tmp.Stat()
	if err != nil {
		return false, 0, err
	}
	if err := tmp.Close(); err != nil {
		return false, 0, err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return false, 0, err
	}
	ok = true
	syncDir(filepath.Dir(dst))
	return merged, st.Size(), nil
}

// copyOldArchive 把已有的会话归档逐帧复制到 w；返回是否存在旧归档。
func (s *Store) copyOldArchive(dst string, w *sessionWriter, have map[string]bool) (bool, error) {
	f, err := os.Open(dst) //nolint:gosec // 路径由已校验的引用派生
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	dec, err := newDecoder(f)
	if err != nil {
		return true, err
	}
	defer dec.Close()
	fr, err := openFrames(dec, sessionMagic)
	if err != nil {
		return true, err
	}
	var buf []byte
	for {
		fx, err := fr.next(buf)
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		if err != nil {
			return true, err
		}
		buf = fx.payload[:0]
		if fx.typ == FrameEntry {
			var eh EntryHeader
			if json.Unmarshal(fx.payload, &eh) == nil {
				have[eh.Ref] = true
			}
		}
		if err := w.frame(fx.typ, fx.kind, fx.seq, fx.payload); err != nil {
			return true, err
		}
	}
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil { //nolint:gosec // 路径由已校验的引用派生
		_ = d.Sync()
		_ = d.Close()
	}
}
