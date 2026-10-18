package audit

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// writeReq 写入一个四阶段齐全的请求：上游请求体在客户端请求体基础上改写，模拟协议转换。
func writeReq(t *testing.T, s *Store, id, session string, at time.Time, body []byte) string {
	t.Helper()
	rec, err := s.Begin(id, session, "", at)
	require.NoError(t, err)
	cr := rec.startSection(KindClientRequest, SectionHeader{Method: "POST", URL: "/v1/messages"})
	cr.body(body)
	cr.end(true, "", nil)
	ur := rec.startSection(KindUpstreamRequest, SectionHeader{Seq: 1, Method: "POST", URL: "https://up/v1/responses"})
	ur.body([]byte(`{"converted":true,"input":`))
	ur.body(body)
	ur.body([]byte(`}`))
	ur.end(true, "", nil)
	up := rec.startSection(KindUpstreamResponse, SectionHeader{Seq: 1, Status: 200})
	up.body([]byte(`{"ok":true,"reply":"` + id + `"}`))
	up.end(true, "", nil)
	cw := rec.startSection(KindClientResponse, SectionHeader{Status: 200})
	cw.body([]byte(`{"wrapped":{"ok":true,"reply":"` + id + `"}}`))
	cw.end(true, "", nil)
	rec.Finish()
	s.pending.Wait()
	require.NotEmpty(t, rec.ArchiveRef())
	return rec.ArchiveRef()
}

// ageActive 把 active 中所有会话目录与文件的 mtime 设为 at，模拟已过去若干天。
func ageActive(t *testing.T, s *Store, at time.Time) {
	t.Helper()
	dirs, err := s.listActiveSessions()
	require.NoError(t, err)
	for _, rel := range dirs {
		dir := filepath.Join(s.ActiveDir(), filepath.FromSlash(rel))
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		for _, e := range entries {
			require.NoError(t, os.Chtimes(filepath.Join(dir, e.Name()), at, at))
		}
		require.NoError(t, os.Chtimes(dir, at, at))
	}
}

// conversation 生成随轮次增长的对话请求体：第 n 轮包含前 n-1 轮的全部内容。
func conversation(turns int, seed int64) [][]byte {
	r := rand.New(rand.NewSource(seed))
	words := strings.Fields("the quick brown fox jumps over lazy dog function return value error handler request response session archive compress stream window buffer index entry reader writer context deadline token model claude gateway upstream account retry")
	var msgs []string
	out := make([][]byte, 0, turns)
	for i := 0; i < turns; i++ {
		var sb strings.Builder
		n := 200 + r.Intn(1500)
		for j := 0; j < n; j++ {
			sb.WriteString(words[r.Intn(len(words))])
			if r.Intn(9) == 0 {
				fmt.Fprintf(&sb, " %d", r.Int63())
			}
			sb.WriteByte(' ')
		}
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, fmt.Sprintf(`{"role":%q,"content":%q}`, role, sb.String()))
		out = append(out, []byte(`{"model":"claude","messages":[`+strings.Join(msgs, ",")+`]}`))
	}
	return out
}

func TestArchiveSessionThenReadFromArchive(t *testing.T) {
	s := newTestStore(t, nil)
	day := time.Date(2026, 10, 1, 10, 0, 0, 0, time.Local)
	bodies := conversation(12, 1)
	var refs []string
	for i, b := range bodies {
		refs = append(refs, writeReq(t, s, fmt.Sprintf("req-%02d", i), "sess-a", day.Add(time.Duration(i)*time.Minute), b))
	}
	other := writeReq(t, s, "other-1", "sess-b", day, []byte(`{"x":1}`))

	// 尚在活跃期：不归档
	rep, err := s.ArchiveBefore(context.Background(), time.Now().Add(-72*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 0, rep.Sessions)
	require.Equal(t, 2, rep.Active)

	ageActive(t, s, time.Now().Add(-96*time.Hour))
	rep, err = s.ArchiveBefore(context.Background(), time.Now().Add(-72*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 2, rep.Sessions, "errors=%v", rep.Errors)
	require.Equal(t, 0, rep.Merged)
	require.Equal(t, 13, rep.Requests)
	require.Empty(t, rep.Errors)

	// active 中的会话目录（含空的日期目录）已删除，archive 中每个会话一个文件
	_, err = os.Stat(filepath.Join(s.ActiveDir(), "2026-10"))
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(s.ArchiveDir(), "2026-10", "01", "sess-a.zst"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(s.ArchiveDir(), "2026-10", "01", "sess-b.zst"))
	require.NoError(t, err)

	for i, ref := range refs {
		av, err := s.ReadArchive(ref)
		require.NoError(t, err)
		require.Equal(t, "complete", av.Status, "ref=%s issues=%v", ref, av.Issues)
		require.Equal(t, LocationArchive, av.Storage.Location)
		require.Equal(t, sha(bodies[i]), av.Files[0].Sections[0].BodySHA256)
		require.True(t, *av.Files[1].Sections[0].HashMatch)
		require.Equal(t, fmt.Sprintf(`{"wrapped":{"ok":true,"reply":"req-%02d"}}`, i), av.Files[3].Sections[0].Body.Text)

		var out bytes.Buffer
		_, err = s.StreamBody(&out, ref, KindClientRequest, 0, false, nil)
		require.NoError(t, err)
		require.Equal(t, bodies[i], out.Bytes())
	}
	av, err := s.ReadArchive(other)
	require.NoError(t, err)
	require.Equal(t, "complete", av.Status)

	// 索引与会话映射不受影响
	entries, err := s.Lookup("req-03")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, refs[3], entries[0].Ref)
}

func TestArchiveReactivatedSessionMerges(t *testing.T) {
	s := newTestStore(t, nil)
	day := time.Date(2026, 10, 1, 10, 0, 0, 0, time.Local)
	bodies := conversation(10, 2)
	var refs []string
	for i, b := range bodies[:6] {
		refs = append(refs, writeReq(t, s, fmt.Sprintf("req-%02d", i), "sess-r", day, b))
	}
	ageActive(t, s, time.Now().Add(-96*time.Hour))
	_, err := s.ArchiveBefore(context.Background(), time.Now().Add(-72*time.Hour))
	require.NoError(t, err)

	// 重启后会话重激活：缓存为空，依旧按标记文件写回 active 中的原层级
	s2, err := NewStore(s.cfg)
	require.NoError(t, err)
	SetDefault(s2)
	later := day.Add(5 * 24 * time.Hour)
	for i, b := range bodies[6:] {
		refs = append(refs, writeReq(t, s2, fmt.Sprintf("req-%02d", i+6), "sess-r", later, b))
	}
	// 重复的请求 ID 不与已归档的引用冲突
	dup := writeReq(t, s2, "req-00", "sess-r", later, []byte(`{"dup":true}`))
	require.Equal(t, "2026-10/01/sess-r/req-00-2", dup)
	for _, ref := range refs[6:] {
		require.True(t, strings.HasPrefix(ref, "2026-10/01/sess-r/"), ref)
		av, err := s2.ReadArchive(ref)
		require.NoError(t, err)
		require.Equal(t, LocationActive, av.Storage.Location)
	}
	// 合并前，旧请求仍可从 archive 读取
	av, err := s2.ReadArchive(refs[0])
	require.NoError(t, err)
	require.Equal(t, LocationArchive, av.Storage.Location)

	ageActive(t, s2, time.Now().Add(-96*time.Hour))
	rep, err := s2.ArchiveBefore(context.Background(), time.Now().Add(-72*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, rep.Sessions)
	require.Equal(t, 1, rep.Merged)
	require.Equal(t, 5, rep.Requests)

	for i, ref := range refs {
		av, err := s2.ReadArchive(ref)
		require.NoError(t, err)
		require.Equal(t, "complete", av.Status, av.Issues)
		require.Equal(t, LocationArchive, av.Storage.Location)
		require.Equal(t, sha(bodies[i]), av.Files[0].Sections[0].BodySHA256)
	}
	av, err = s2.ReadArchive(dup)
	require.NoError(t, err)
	require.Equal(t, `{"dup":true}`, av.Files[0].Sections[0].Body.Text)
	require.Equal(t, 11, countEntries(t, s2, "2026-10/01/sess-r"))
}

// countEntries 统计会话归档中的请求条目数。
func countEntries(t *testing.T, s *Store, rel string) int {
	t.Helper()
	f, err := os.Open(s.archiveSessionPath(rel))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	dec, err := newDecoder(f)
	require.NoError(t, err)
	defer dec.Close()
	fr, err := openFrames(dec, sessionMagic)
	require.NoError(t, err)
	n := 0
	for {
		fx, err := fr.next(nil)
		if err == io.EOF {
			return n
		}
		require.NoError(t, err)
		if fx.typ == FrameEntry {
			n++
		}
	}
}

// 归档 rename 之后、删除 active 之前崩溃：重做时按引用去重，不重复写入。
func TestArchiveCrashBeforeDeleteIsIdempotent(t *testing.T) {
	s := newTestStore(t, nil)
	day := time.Date(2026, 9, 1, 10, 0, 0, 0, time.Local)
	for i, b := range conversation(4, 3) {
		writeReq(t, s, fmt.Sprintf("c-%d", i), "sess-c", day, b)
	}
	rel := "2026-09/01/sess-c"
	dir := filepath.Join(s.ActiveDir(), filepath.FromSlash(rel))
	backup := t.TempDir()
	require.NoError(t, os.CopyFS(backup, os.DirFS(dir)))

	ageActive(t, s, time.Now().Add(-96*time.Hour))
	_, err := s.ArchiveBefore(context.Background(), time.Now().Add(-72*time.Hour))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.CopyFS(dir, os.DirFS(backup)))
	ageActive(t, s, time.Now().Add(-96*time.Hour))

	rep, err := s.ArchiveBefore(context.Background(), time.Now().Add(-72*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, rep.Merged)
	require.Equal(t, 4, countEntries(t, s, rel))
	_, err = os.Stat(dir)
	require.True(t, os.IsNotExist(err))
}

func TestArchiveSkipsBusySessionAndKeepsCorruptArchive(t *testing.T) {
	s := newTestStore(t, nil)
	day := time.Date(2026, 9, 2, 10, 0, 0, 0, time.Local)
	writeReq(t, s, "b-1", "sess-busy", day, []byte(`{"a":1}`))
	writeReq(t, s, "k-1", "sess-bad", day, []byte(`{"a":2}`))
	ageActive(t, s, time.Now().Add(-96*time.Hour))

	// 会话正在写入（持有写入锁）：跳过
	unlock := s.locks.RLock("2026-09/02/sess-busy")
	// 已有归档损坏：放弃合并，active 保持不动
	bad := s.archiveSessionPath("2026-09/02/sess-bad")
	require.NoError(t, os.MkdirAll(filepath.Dir(bad), 0o750))
	require.NoError(t, os.WriteFile(bad, []byte("not a zstd stream"), 0o640))

	rep, err := s.ArchiveBefore(context.Background(), time.Now().Add(-72*time.Hour))
	require.NoError(t, err)
	unlock()
	require.Equal(t, 1, rep.Busy)
	require.Equal(t, 1, rep.Failed)
	require.Equal(t, 0, rep.Sessions)
	_, err = os.Stat(s.activeRequestPath("2026-09/02/sess-bad/k-1-1"))
	require.NoError(t, err, "失败时 active 中的数据不能丢")
	b, _ := os.ReadFile(bad)
	require.Equal(t, "not a zstd stream", string(b), "失败时旧归档保持原样")
	entries, _ := os.ReadDir(filepath.Dir(bad))
	require.Len(t, entries, 1, "临时文件已清理")

	rep, err = s.ArchiveBefore(context.Background(), time.Now().Add(-72*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, rep.Sessions, "锁释放后下一轮正常归档")
}

func TestArchiveTruncatedRequestKeepsCompleteFrames(t *testing.T) {
	s := newTestStore(t, nil)
	day := time.Date(2026, 9, 3, 10, 0, 0, 0, time.Local)
	ok := writeReq(t, s, "t-ok", "sess-t", day, []byte(`{"fine":true}`))
	bad := writeReq(t, s, "t-bad", "sess-t", day, bytes.Repeat([]byte(`{"x":"abcdefgh"}`), 50000))
	p := s.activeRequestPath(bad)
	st, err := os.Stat(p)
	require.NoError(t, err)
	require.NoError(t, os.Truncate(p, st.Size()/2))
	ageActive(t, s, time.Now().Add(-96*time.Hour))
	rep, err := s.ArchiveBefore(context.Background(), time.Now().Add(-72*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, rep.Sessions, rep.Errors)

	av, err := s.ReadArchive(ok)
	require.NoError(t, err)
	require.Equal(t, "complete", av.Status)
	av, err = s.ReadArchive(bad)
	require.NoError(t, err)
	require.Equal(t, LocationArchive, av.Storage.Location)
	require.Equal(t, "incomplete", av.Status)
	require.Equal(t, "truncated", av.Storage.Status)
}

func TestRequestBundleActiveAndArchive(t *testing.T) {
	s := newTestStore(t, nil)
	day := time.Date(2026, 9, 4, 10, 0, 0, 0, time.Local)
	body := []byte(`{"hello":"bundle"}`)
	ref := writeReq(t, s, "bundle-1", "sess-bundle", day, body)

	var active bytes.Buffer
	info, err := s.WriteRequestBundle(&active, ref)
	require.NoError(t, err)
	require.Equal(t, LocationActive, info.Location)
	file, _ := os.ReadFile(s.activeRequestPath(ref))
	require.Equal(t, file, active.Bytes(), "active 命中时原样发送文件字节")

	ageActive(t, s, time.Now().Add(-96*time.Hour))
	_, err = s.ArchiveBefore(context.Background(), time.Now().Add(-72*time.Hour))
	require.NoError(t, err)
	var archived bytes.Buffer
	info, err = s.WriteRequestBundle(&archived, ref)
	require.NoError(t, err)
	require.Equal(t, LocationArchive, info.Location)

	// 两种来源解压后的内容逐字节一致
	decode := func(b []byte) []byte {
		dec, err := newDecoder(bytes.NewReader(b))
		require.NoError(t, err)
		defer dec.Close()
		out, err := io.ReadAll(dec)
		require.NoError(t, err)
		return out
	}
	require.Equal(t, decode(active.Bytes()), decode(archived.Bytes()))

	var missing bytes.Buffer
	_, err = s.WriteRequestBundle(&missing, "2026-09/04/sess-bundle/none-1")
	require.ErrorIs(t, err, errRequestNotFound)
}

func TestNextClock(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	require.Equal(t, time.Date(2026, 10, 18, 3, 0, 0, 0, loc), nextClock(time.Date(2026, 10, 18, 2, 59, 0, 0, loc), 3, 0))
	require.Equal(t, time.Date(2026, 10, 19, 3, 0, 0, 0, loc), nextClock(time.Date(2026, 10, 18, 3, 0, 0, 0, loc), 3, 0))
	require.Equal(t, time.Date(2026, 10, 19, 3, 0, 0, 0, loc), nextClock(time.Date(2026, 10, 18, 21, 51, 0, 0, loc), 3, 0))
	h, m, err := parseClock("03:30")
	require.NoError(t, err)
	require.Equal(t, [2]int{3, 30}, [2]int{h, m})
}

// TestArchiveCompressionRatio 对比“每请求单独压缩（active）”与“会话整体压缩（archive）”。
func TestArchiveCompressionRatio(t *testing.T) {
	s := newTestStore(t, nil)
	day := time.Date(2026, 8, 1, 10, 0, 0, 0, time.Local)
	turns := 60
	if v := os.Getenv("AUDIT_RATIO_TURNS"); v != "" {
		_, _ = fmt.Sscan(v, &turns)
	}
	bodies := conversation(turns, 7)
	var raw int64
	var refs []string
	for i, b := range bodies {
		raw += int64(2 * len(b))
		refs = append(refs, writeReq(t, s, fmt.Sprintf("q-%03d", i), "sess-ratio", day, b))
	}
	var activeBytes int64
	for _, ref := range refs {
		st, err := os.Stat(s.activeRequestPath(ref))
		require.NoError(t, err)
		activeBytes += st.Size()
	}
	ageActive(t, s, time.Now().Add(-96*time.Hour))
	start := time.Now()
	rep, err := s.ArchiveBefore(context.Background(), time.Now().Add(-72*time.Hour))
	require.NoError(t, err)
	archiveElapsed := time.Since(start)
	st, err := os.Stat(s.archiveSessionPath("2026-08/01/sess-ratio"))
	require.NoError(t, err)

	t0 := time.Now()
	av, err := s.ReadArchive(refs[len(refs)-1])
	require.NoError(t, err)
	readLast := time.Since(t0)
	require.Equal(t, "complete", av.Status)

	t.Logf("RATIO requests=%d raw_bodies=%.1fMB active_total=%.1fKB archive=%.1fKB active/archive=%.1fx raw/archive=%.0fx archive_time=%s read_last=%s",
		len(bodies), float64(raw)/(1<<20), float64(activeBytes)/1024, float64(st.Size())/1024,
		float64(activeBytes)/float64(st.Size()), float64(raw)/float64(st.Size()), archiveElapsed.Round(time.Millisecond), readLast.Round(time.Millisecond))
	require.Equal(t, rep.ActiveBytes, activeBytes)
	require.Less(t, st.Size()*5, activeBytes, "会话整体压缩应显著小于逐请求压缩")
}
