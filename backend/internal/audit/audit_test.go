package audit

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const (
	clientSecret   = "sk-client-SECRETVALUE-1234567890abcd"
	upstreamSecret = "sk-upstream-TOPSECRET-0987654321wxyz"
)

func newTestStore(t *testing.T, mutate func(*Config)) *Store {
	t.Helper()
	cfg := DefaultConfig()
	cfg.RootDir = t.TempDir()
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := NewStore(cfg)
	require.NoError(t, err)
	SetDefault(s)
	t.Cleanup(func() { SetDefault(nil) })
	return s
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func waitFlush(t *testing.T, s *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.pending.Wait()
	_ = ctx
}

// decompressedAll 解压请求文件（四个阶段），用于检查鉴权原值是否泄漏。
func decompressedAll(t *testing.T, s *Store, ref string) []byte {
	t.Helper()
	var all bytes.Buffer
	f, err := os.Open(s.activeRequestPath(ref))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	dec, err := newDecoder(f)
	require.NoError(t, err)
	defer dec.Close()
	_, err = io.Copy(&all, dec)
	require.NoError(t, err)
	return all.Bytes()
}

type upstreamCapture struct {
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
}

func (u *upstreamCapture) record(r *http.Request) []byte {
	b, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.bodies = append(u.bodies, b)
	u.headers = append(u.headers, r.Header.Clone())
	u.mu.Unlock()
	return b
}

func testRouter(upstreamURL string, failFirst bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	ident := func(c *gin.Context) map[string]any {
		return map[string]any{"api_key_id": int64(42), "user_id": int64(9)}
	}
	passthrough := func(c *gin.Context) { c.Next() }
	r.POST("/v1/messages", WrapHandler(passthrough, ident), func(c *gin.Context) {
		in, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.AbortWithStatus(400)
			return
		}
		// 模拟协议转换：客户端 Anthropic 格式 → 上游 Responses 格式
		converted := []byte(`{"converted":true,"input":` + string(in) + `}`)
		stream := c.Query("stream") == "1"
		accounts := []int64{101}
		if failFirst {
			accounts = []int64{100, 101} // 账户切换
		}
		var resp *http.Response
		for i, acc := range accounts {
			req, _ := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstreamURL+"/v1/responses?key=gemini-query-secret-abcdef&attempt="+fmt.Sprint(i), bytes.NewReader(converted))
			req.Header.Set("Authorization", "Bearer "+upstreamSecret)
			req.Header.Set("User-Agent", "rewritten-ua/1.0")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept-Encoding", "gzip")
			if stream {
				req.Header.Set("Accept", "text/event-stream")
			}
			client := WrapClient(&http.Client{}, AttemptInfo{AccountID: acc, Via: "test"})
			resp, err = client.Do(req)
			if err != nil {
				c.AbortWithStatus(502)
				return
			}
			if resp.StatusCode >= 500 && i < len(accounts)-1 {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				continue
			}
			break
		}
		defer func() { _ = resp.Body.Close() }()
		var body io.Reader = resp.Body
		if resp.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(resp.Body)
			if err != nil {
				c.AbortWithStatus(502)
				return
			}
			body = gz
		}
		if stream {
			c.Header("Content-Type", "text/event-stream")
			c.Status(200)
			sc := bufio.NewScanner(body)
			for sc.Scan() {
				_, _ = c.Writer.WriteString(sc.Text() + "\n")
				c.Writer.Flush()
			}
			return
		}
		out, _ := io.ReadAll(body)
		c.Data(resp.StatusCode, "application/json", append([]byte(`{"wrapped":`), append(out, '}')...))
	})
	return r
}

func newUpstream(t *testing.T, cap *upstreamCapture, failFirst bool) *httptest.Server {
	var n int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		mu.Lock()
		n++
		cur := n
		mu.Unlock()
		if failFirst && cur == 1 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":"overloaded"}`))
			return
		}
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			for i := 0; i < 5; i++ {
				_, _ = fmt.Fprintf(w, "event: delta\ndata: {\"i\":%d}\n\n", i)
				fl.Flush()
			}
			_, _ = fmt.Fprint(w, "event: message_stop\ndata: {}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Set-Cookie", "session=upstreamcookievalue123456; Path=/")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte(`{"ok":true,"text":"你好"}`))
		_ = gz.Close()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func doClient(t *testing.T, r http.Handler, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+clientSecret)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func onlyRef(t *testing.T, s *Store, session string) string {
	t.Helper()
	var refs []string
	root := s.ActiveDir()
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".zst") {
			rel, _ := filepath.Rel(root, p)
			refs = append(refs, strings.TrimSuffix(filepath.ToSlash(rel), ".zst"))
		}
		return nil
	})
	require.Len(t, refs, 1, "refs=%v", refs)
	if session != "" {
		require.Contains(t, refs[0], "/"+session+"/")
	}
	return refs[0]
}

func TestMaskSecret(t *testing.T) {
	require.Equal(t, "Bearer sk-c****************************abcd", MaskSecret("Bearer "+clientSecret))
	require.Equal(t, "********", MaskSecret("12345678"))
	require.Equal(t, "abcd*xyzw", MaskSecret("abcd1xyzw"))
	red := newRedactor(DefaultConfig())
	h := red.Header(http.Header{"Cookie": {"a=verysecretvalue1; b=x"}, "X-Other": {"keep-me-unchanged"}})
	require.Equal(t, "a=very********lue1; b=*", h["Cookie"][0])
	require.Equal(t, "keep-me-unchanged", h["X-Other"][0])
}

func TestHTTPArchiveFourKindsIntegrityAndRedaction(t *testing.T) {
	s := newTestStore(t, nil)
	cap := &upstreamCapture{}
	up := newUpstream(t, cap, false)
	r := testRouter(up.URL, false)

	body := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi 你好"}]}`)
	w := doClient(t, r, "/v1/messages", body, map[string]string{"X-Claude-Code-Session-Id": "sess-abc"})
	require.Equal(t, 200, w.Code)
	waitFlush(t, s)

	ref := onlyRef(t, s, "sess-abc")
	av, err := s.ReadArchive(ref)
	require.NoError(t, err)
	require.Equal(t, "complete", av.Status, "issues=%v", av.Issues)
	require.Len(t, av.Files, 4)

	cr := av.Files[0].Sections[0]
	require.Equal(t, sha(body), cr.BodySHA256)
	require.True(t, *cr.HashMatch)
	require.Equal(t, "Bearer sk-c****************************abcd", cr.Header.Header["Authorization"][0])

	ur := av.Files[1].Sections[0]
	require.Equal(t, sha(cap.bodies[0]), ur.BodySHA256, "上游实际收到的字节与归档一致")
	require.Equal(t, "rewritten-ua/1.0", ur.Header.Header["User-Agent"][0])
	require.Contains(t, ur.Header.URL, "key=gemi")
	require.NotContains(t, ur.Header.URL, "gemini-query-secret-abcdef")
	require.EqualValues(t, 101, ur.Header.Meta["account_id"])

	uresp := av.Files[2].Sections[0]
	require.Equal(t, "gzip", uresp.Body.ContentEncoding)
	require.True(t, uresp.Body.Decoded)
	require.Equal(t, `{"ok":true,"text":"你好"}`, uresp.Body.Text)

	cw := av.Files[3].Sections[0]
	require.Equal(t, sha(w.Body.Bytes()), cw.BodySHA256, "客户端实际收到的字节与归档一致")
	require.Equal(t, 200, cw.Header.Status)
	require.EqualValues(t, 42, cw.End.Meta["api_key_id"])

	all := decompressedAll(t, s, ref)
	for _, secret := range []string{clientSecret, upstreamSecret, "gemini-query-secret-abcdef", "upstreamcookievalue123456"} {
		require.False(t, bytes.Contains(all, []byte(secret)), "secret leaked: %s", secret)
	}
	// 真实调用未被脱敏影响
	require.Equal(t, "Bearer "+upstreamSecret, cap.headers[0].Get("Authorization"))

	// 下载：原始字节与上游发送的 gzip 字节逐字节一致；decode=1 为解码后内容
	var raw, decoded bytes.Buffer
	_, err = s.StreamBody(&raw, ref, KindUpstreamResponse, 0, false, nil)
	require.NoError(t, err)
	require.Equal(t, uresp.BodySHA256, sha(raw.Bytes()))
	_, err = s.StreamBody(&decoded, ref, KindUpstreamResponse, 0, true, nil)
	require.NoError(t, err)
	require.Equal(t, `{"ok":true,"text":"你好"}`, decoded.String())
}

func TestSSEAndRetryAccountSwitch(t *testing.T) {
	s := newTestStore(t, nil)
	cap := &upstreamCapture{}
	up := newUpstream(t, cap, true)
	r := testRouter(up.URL, true)
	w := doClient(t, r, "/v1/messages?stream=1", []byte(`{"stream":true}`), map[string]string{"session_id": "codex-1"})
	require.Equal(t, 200, w.Code)
	waitFlush(t, s)
	ref := onlyRef(t, s, "codex-1")
	av, err := s.ReadArchive(ref)
	require.NoError(t, err)
	require.Equal(t, "complete", av.Status, "issues=%v", av.Issues)
	require.Len(t, av.Attempts, 2)
	require.Equal(t, 503, av.Attempts[0].Status)
	require.EqualValues(t, 100, av.Attempts[0].AccountID)
	require.Equal(t, 200, av.Attempts[1].Status)
	require.EqualValues(t, 101, av.Attempts[1].AccountID)
	cw := av.Files[3].Sections[0]
	require.Equal(t, "sse", cw.Header.Transport)
	require.Len(t, cw.Body.SSE, 6)
	require.True(t, *cw.Body.SSEDone)
	require.Equal(t, sha(w.Body.Bytes()), cw.BodySHA256)
	// 第二次尝试的上游响应同样是 SSE 且正常终止
	resp2 := av.Files[2].Sections[1]
	require.True(t, *resp2.Body.SSEDone)
}

func TestSessionDirFixedAcrossDaysAndRestart(t *testing.T) {
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.RootDir = root
	s1, err := NewStore(cfg)
	require.NoError(t, err)
	day1 := time.Date(2026, 1, 31, 23, 59, 0, 0, time.Local)
	day2 := day1.Add(2 * time.Minute)
	r1, err := s1.Begin("req-1", "sess-x", "", day1)
	require.NoError(t, err)
	r1.Finish()
	r2, err := s1.Begin("req-2", "sess-x", "", day2)
	require.NoError(t, err)
	r2.Finish()
	s1.pending.Wait()
	require.Equal(t, "2026-01/31/sess-x/req-1-1", r1.ArchiveRef())
	require.Equal(t, "2026-01/31/sess-x/req-2-1", r2.ArchiveRef())

	// 重启：新 Store 缓存为空，依旧定位到首次日期目录（读取标记文件，不扫描）
	s2, err := NewStore(cfg)
	require.NoError(t, err)
	r3, err := s2.Begin("req-3", "sess-x", "", day2.Add(48*time.Hour))
	require.NoError(t, err)
	r3.Finish()
	s2.pending.Wait()
	require.Equal(t, "2026-01/31/sess-x/req-3-1", r3.ArchiveRef())

	// 无会话：当天 nosession
	r4, err := s2.Begin("req-4", "", "", day2)
	require.NoError(t, err)
	r4.Finish()
	s2.pending.Wait()
	require.Equal(t, "2026-02/01/nosession/req-4-1", r4.ArchiveRef())

	// 非法会话字符被哈希，不会逃逸目录
	r5, err := s2.Begin("../../etc", "../../evil session", "", day2)
	require.NoError(t, err)
	r5.Finish()
	s2.pending.Wait()
	require.True(t, ValidRef(r5.ArchiveRef()), r5.ArchiveRef())
	require.True(t, strings.HasPrefix(r5.ArchiveRef(), "2026-02/01/h-"))
	s1.pending.Wait()
	s2.pending.Wait()
}

func TestDuplicateRequestIDNeverOverwrites(t *testing.T) {
	s := newTestStore(t, nil)
	now := time.Now()
	var refs []string
	for i := 0; i < 3; i++ {
		rec, err := s.Begin("dup-id", "", "", now)
		require.NoError(t, err)
		sec := rec.startSection(KindClientRequest, SectionHeader{})
		sec.body([]byte(fmt.Sprintf("payload-%d", i)))
		sec.end(true, "", nil)
		rec.Finish()
		s.pending.Wait()
		refs = append(refs, rec.ArchiveRef())
	}
	s.pending.Wait()
	require.True(t, strings.HasSuffix(refs[0], "dup-id-1"))
	require.True(t, strings.HasSuffix(refs[2], "dup-id-3"))
	for i, ref := range refs {
		av, err := s.ReadArchive(ref)
		require.NoError(t, err)
		require.Equal(t, fmt.Sprintf("payload-%d", i), av.Files[0].Sections[0].Body.Text)
	}
	entries, err := s.Lookup("dup-id")
	require.NoError(t, err)
	require.Len(t, entries, 3)
	cands, err := s.Resolve(&UsageRef{RequestID: "client:dup-id"})
	require.NoError(t, err)
	require.Len(t, cands, 3, "重复 ID 以候选列表呈现为关联歧义")
}

func TestLargeBodyStreamingModeIntegrity(t *testing.T) {
	s := newTestStore(t, func(c *Config) { c.MemoryBufferBytes = 64 << 10; c.DisplayBodyLimit = 1 << 20 })
	big := make([]byte, 6<<20)
	_, _ = rand.Read(big)
	rec, err := s.Begin("big", "", "", time.Now())
	require.NoError(t, err)
	sec := rec.startSection(KindUpstreamRequest, SectionHeader{Seq: 1})
	for off := 0; off < len(big); off += 32 << 10 {
		sec.body(big[off:min(off+32<<10, len(big))])
	}
	sec.end(true, "", nil)
	rec.Finish()
	s.pending.Wait()
	av, err := s.ReadArchive(rec.ArchiveRef())
	require.NoError(t, err)
	got := av.Files[1].Sections[0]
	require.EqualValues(t, len(big), got.BodyBytes)
	require.Equal(t, sha(big), got.BodySHA256)
	require.True(t, *got.HashMatch)
	require.True(t, got.Body.TruncatedForUI)
	var out bytes.Buffer
	_, err = s.StreamBody(&out, rec.ArchiveRef(), KindUpstreamRequest, 0, false, nil)
	require.NoError(t, err)
	require.True(t, bytes.Equal(big, out.Bytes()))
}

func TestTruncatedAndEmptyArchives(t *testing.T) {
	s := newTestStore(t, nil)
	rec, err := s.Begin("trunc", "", "", time.Now())
	require.NoError(t, err)
	sec := rec.startSection(KindClientRequest, SectionHeader{})
	sec.body(bytes.Repeat([]byte("x"), 100000))
	sec.end(true, "", nil)
	rec.Finish()
	s.pending.Wait()
	p := s.activeRequestPath(rec.ArchiveRef())
	st, _ := os.Stat(p)
	require.NoError(t, os.Truncate(p, st.Size()/2))
	av, err := s.ReadArchive(rec.ArchiveRef())
	require.NoError(t, err)
	require.NotEqual(t, "complete", av.Status)
	require.Contains(t, []string{"truncated", "corrupt"}, av.Files[0].Status)
	require.NotEmpty(t, av.Issues)

	// 已创建但未落盘的空文件：视为进行中。
	rec2, err := s.Begin("empty", "", "", time.Now())
	require.NoError(t, err)
	rec2.Finish()
	s.pending.Wait()
	require.NoError(t, os.Truncate(s.activeRequestPath(rec2.ArchiveRef()), 0))
	av, err = s.ReadArchive(rec2.ArchiveRef())
	require.NoError(t, err)
	require.Equal(t, "in_progress", av.Status)
	require.Equal(t, "empty", av.Files[3].Status)

	av2, err := s.ReadArchive("2026-01/01/nosession/none-1")
	require.NoError(t, err)
	require.Equal(t, "missing", av2.Status)
}

func TestSessionCacheBounded(t *testing.T) {
	s := newTestStore(t, func(c *Config) { c.SessionCacheSize = 16 })
	for i := 0; i < 200; i++ {
		rec, err := s.Begin(fmt.Sprintf("r%d", i), fmt.Sprintf("s%d", i), "", time.Now())
		require.NoError(t, err)
		rec.Finish()
	}
	s.pending.Wait()
	require.LessOrEqual(t, s.sessions.len(), 16)
}

func TestUsageCallbackIndexAndStates(t *testing.T) {
	s := newTestStore(t, nil)
	rec, err := s.Begin("cid-1", "", "", time.Now())
	require.NoError(t, err)
	ctx := WithRecorder(context.Background(), rec)
	OnUsage(ctx, UsageEvent{RequestID: "client:cid-1", APIKeyID: 5, AccountID: 6, Inserted: true, InTx: true})
	OnUsage(ctx, UsageEvent{RequestID: "web_search:xyz", APIKeyID: 5, Inserted: true})
	rec.Finish()
	s.pending.Wait()
	entries, err := s.Lookup("client:cid-1")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, UsageStatePendingTx, entries[0].UsageState, "事务内回调不得冒充提交成功")
	cands, err := s.Resolve(&UsageRef{RequestID: "web_search:xyz", APIKeyID: 5})
	require.NoError(t, err)
	require.Len(t, cands, 1)
	require.Equal(t, rec.ArchiveRef(), cands[0].Ref)
	cands, err = s.Resolve(&UsageRef{RequestID: "client:cid-1", APIKeyID: 5, AccountID: 6})
	require.NoError(t, err)
	require.Len(t, cands, 1)
	require.Contains(t, cands[0].MatchReason, "api_key_id")
}

func TestDisabledPassthrough(t *testing.T) {
	SetDefault(nil)
	called := false
	h := WrapHandler(func(c *gin.Context) { called = true }, nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	h(c)
	require.True(t, called)
	cl := &http.Client{}
	require.Same(t, cl, WrapClient(cl, AttemptInfo{}))
	require.Nil(t, NewWSTap(context.Background()))
}

// TestNoTempFilesOnRead 读取与下载不产生任何临时文件。
func TestNoTempFilesOnRead(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	s := newTestStore(t, func(c *Config) { c.MemoryBufferBytes = 1024 })
	rec, err := s.Begin("tmpcheck", "", "", time.Now())
	require.NoError(t, err)
	sec := rec.startSection(KindUpstreamResponse, SectionHeader{Seq: 1, Header: map[string][]string{"Content-Encoding": {"gzip"}}})
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(bytes.Repeat([]byte("hello world "), 50000))
	_ = zw.Close()
	sec.body(gz.Bytes())
	sec.end(true, "", nil)
	rec.Finish()
	s.pending.Wait()
	before := listAll(t, s.Root())
	_, err = s.ReadArchive(rec.ArchiveRef())
	require.NoError(t, err)
	var out bytes.Buffer
	_, err = s.StreamBody(&out, rec.ArchiveRef(), KindUpstreamResponse, 0, true, nil)
	require.NoError(t, err)
	require.Equal(t, 600000, out.Len())
	require.Equal(t, before, listAll(t, s.Root()))
	entries, _ := os.ReadDir(tmp)
	require.Empty(t, entries)
}

func listAll(t *testing.T, root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// 跨日 + 乱序完成：先开始（23:59）的长请求晚结束，后开始（00:01）的请求先结束，
// 会话仍固定在最先开始请求的日期目录。
func TestSessionDateFixedByFirstBeginEvenIfFinishedLater(t *testing.T) {
	s := newTestStore(t, nil)
	day1 := time.Date(2026, 3, 31, 23, 59, 0, 0, time.Local)
	long, err := s.Begin("long", "sess-order", "", day1)
	require.NoError(t, err)
	short, err := s.Begin("short", "sess-order", "", day1.Add(2*time.Minute))
	require.NoError(t, err)
	short.Finish()
	require.Eventually(t, func() bool { return short.ArchiveRef() != "" }, 5*time.Second, 5*time.Millisecond)
	long.Finish()
	s.pending.Wait()
	require.Equal(t, "2026-03/31/sess-order/short-1", short.ArchiveRef())
	require.Equal(t, "2026-03/31/sess-order/long-1", long.ArchiveRef())
}

// 异步计费在请求结束之后才回调：仍能补写索引并被解析到同一归档。
func TestUsageCallbackAfterFinish(t *testing.T) {
	s := newTestStore(t, nil)
	rec, err := s.Begin("late-cid", "", "", time.Now())
	require.NoError(t, err)
	ctx := WithRecorder(context.Background(), rec)
	rec.Finish()
	OnUsage(ctx, UsageEvent{RequestID: "grok-video:abc", APIKeyID: 3, Inserted: true})
	s.pending.Wait()
	cands, err := s.Resolve(&UsageRef{RequestID: "grok-video:abc", APIKeyID: 3})
	require.NoError(t, err)
	require.Len(t, cands, 1)
	require.Equal(t, rec.ArchiveRef(), cands[0].Ref)
	require.Contains(t, cands[0].MatchReason, "usage_callback_request_id")
}
