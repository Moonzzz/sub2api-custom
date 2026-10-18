package audit

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func benchStore(b *testing.B, mutate func(*Config)) *Store {
	b.Helper()
	cfg := DefaultConfig()
	cfg.RootDir = b.TempDir()
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := NewStore(cfg)
	if err != nil {
		b.Fatal(err)
	}
	return s
}

// archiveOne 模拟一次完整请求：四份文件各写一段正文。
func archiveOne(s *Store, id, session string, reqBody, respBody []byte, chunks int) error {
	rec, err := s.Begin(id, session, "", time.Now())
	if err != nil {
		return err
	}
	for _, kind := range Kinds {
		seq := 0
		if kind == KindUpstreamRequest || kind == KindUpstreamResponse {
			seq = 1
		}
		sec := rec.startSection(kind, SectionHeader{Seq: seq, Header: map[string][]string{"Content-Type": {"application/json"}}})
		body := reqBody
		if kind == KindUpstreamResponse || kind == KindClientResponse {
			body = respBody
		}
		if chunks <= 1 {
			sec.body(body)
		} else {
			step := max(1, len(body)/chunks)
			for off := 0; off < len(body); off += step {
				sec.body(body[off:min(off+step, len(body))])
			}
		}
		sec.end(true, "", nil)
	}
	rec.Finish()
	return nil
}

func jsonish(n int) []byte {
	var buf bytes.Buffer
	for buf.Len() < n {
		fmt.Fprintf(&buf, `{"role":"user","content":"line %d lorem ipsum dolor sit amet, consectetur adipiscing"},`, buf.Len())
	}
	return buf.Bytes()[:n]
}

func BenchmarkArchiveSmallRequest(b *testing.B) {
	s := benchStore(b, nil)
	req, resp := jsonish(4<<10), jsonish(2<<10)
	b.SetBytes(int64(2*len(req) + 2*len(resp)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := archiveOne(s, fmt.Sprintf("r%d", i), "bench-sess", req, resp, 1); err != nil {
			b.Fatal(err)
		}
	}
	s.pending.Wait()
}

func BenchmarkArchiveLargeRequestParallel(b *testing.B) {
	s := benchStore(b, nil)
	req, resp := jsonish(1<<20), jsonish(64<<10)
	b.SetBytes(int64(2*len(req) + 2*len(resp)))
	var n atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := n.Add(1)
			if err := archiveOne(s, fmt.Sprintf("p%d", i), fmt.Sprintf("s%d", i%64), req, resp, 64); err != nil {
				b.Error(err)
			}
		}
	})
	s.pending.Wait()
}

func BenchmarkSSEStreamChunks(b *testing.B) {
	s := benchStore(b, nil)
	event := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello world\"}}\n\n")
	rec, err := s.Begin("sse", "", "", time.Now())
	require.NoError(b, err)
	sec := rec.startSection(KindClientResponse, SectionHeader{})
	b.SetBytes(int64(len(event)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sec.body(event)
	}
	b.StopTimer()
	sec.end(true, "", nil)
	rec.Finish()
	s.pending.Wait()
}

// BenchmarkGatewayOverhead 对比同一 Gin 处理链开启/关闭归档时请求路径的耗时（含 RoundTripper、
// 四份报文采集），后台压缩落盘不计入。
func BenchmarkGatewayOverhead(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jsonish(8 << 10))
	}))
	defer upstream.Close()
	body := jsonish(32 << 10)
	for _, enabled := range []bool{false, true} {
		b.Run(fmt.Sprintf("archive=%v", enabled), func(b *testing.B) {
			if enabled {
				SetDefault(benchStore(b, nil))
			} else {
				SetDefault(nil)
			}
			defer SetDefault(nil)
			gin.SetMode(gin.ReleaseMode)
			r := gin.New()
			r.POST("/v1/messages", WrapHandler(func(c *gin.Context) { c.Next() }, nil), func(c *gin.Context) {
				in, _ := io.ReadAll(c.Request.Body)
				req, _ := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstream.URL, bytes.NewReader(in))
				resp, err := WrapClient(http.DefaultClient, AttemptInfo{AccountID: 1}).Do(req)
				if err != nil {
					c.AbortWithStatus(502)
					return
				}
				defer func() { _ = resp.Body.Close() }()
				out, _ := io.ReadAll(resp.Body)
				c.Data(200, "application/json", out)
			})
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)))
				if w.Code != 200 {
					b.Fatal(w.Code)
				}
				// 只计请求路径耗时：后台落盘在计时外排空（落盘吞吐见 TestManyFilesLocatePerformance）。
				if s := Default(); s != nil {
					b.StopTimer()
					s.pending.Wait()
					b.StartTimer()
				}
			}
			b.StopTimer()
			if s := Default(); s != nil {
				s.pending.Wait()
			}
		})
	}
}

// TestManyFilesLocatePerformance 大量文件场景：写入 N 个请求（多会话 + nosession），
// 统计 Begin（含目录定位）与按 ID 查找的延迟分位数，验证查找不随历史文件数增长（不扫描目录）。
// 规模通过 AUDIT_PERF_N 调整（默认 3000，-short 下跳过）。
func TestManyFilesLocatePerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("perf")
	}
	n := 3000
	if v := os.Getenv("AUDIT_PERF_N"); v != "" {
		_, _ = fmt.Sscan(v, &n)
	}
	s := newTestStore(t, nil)
	req := jsonish(2 << 10)
	var beginLat []time.Duration
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			session := fmt.Sprintf("sess-%d", i%200)
			if i%10 == 0 {
				session = ""
			}
			t0 := time.Now()
			rec, err := s.Begin(fmt.Sprintf("req-%06d", i), session, "", time.Now())
			d := time.Since(t0)
			require.NoError(t, err)
			sec := rec.startSection(KindClientRequest, SectionHeader{})
			sec.body(req)
			sec.end(true, "", nil)
			rec.Finish()
			mu.Lock()
			beginLat = append(beginLat, d)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	s.pending.Wait()
	elapsed := time.Since(start)

	var files int
	_ = filepath.WalkDir(s.Root(), func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files++
		}
		return nil
	})
	var lookupLat []time.Duration
	for i := 0; i < 500; i++ {
		id := fmt.Sprintf("req-%06d", (i*7919)%n)
		t0 := time.Now()
		entries, err := s.Lookup(id)
		lookupLat = append(lookupLat, time.Since(t0))
		require.NoError(t, err)
		require.Len(t, entries, 1)
		_, err = s.ReadArchive(entries[0].Ref)
		require.NoError(t, err)
	}
	// 重启后会话定位：新 Store，缓存为空，只读标记文件。
	s2, err := NewStore(s.cfg)
	require.NoError(t, err)
	t0 := time.Now()
	for i := 0; i < 200; i++ {
		_, err := s2.sessions.Resolve(fmt.Sprintf("sess-%d", i), time.Now().Add(72*time.Hour))
		require.NoError(t, err)
	}
	coldResolve := time.Since(t0) / 200

	pct := func(d []time.Duration, p float64) time.Duration {
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		return d[int(float64(len(d)-1)*p)]
	}
	t.Logf("PERF requests=%d files=%d wall=%s throughput=%.0f req/s", n, files, elapsed.Round(time.Millisecond), float64(n)/elapsed.Seconds())
	t.Logf("PERF begin p50=%s p95=%s p99=%s", pct(beginLat, .5), pct(beginLat, .95), pct(beginLat, .99))
	t.Logf("PERF lookup p50=%s p99=%s cold_session_resolve_avg=%s", pct(lookupLat, .5), pct(lookupLat, .99), coldResolve)
}
