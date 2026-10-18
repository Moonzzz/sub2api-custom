package audit

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func apiRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterRoutes(r.Group("/admin"), nil)
	return r
}

func apiGet(t *testing.T, r http.Handler, path string, q url.Values) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path+"?"+q.Encode(), nil))
	return w
}

func TestBodyDownloadFileExtension(t *testing.T) {
	require.Equal(t, ".json", bodyFileExt(map[string][]string{"Content-Type": {"application/json"}}, false))
	require.Equal(t, ".json", bodyFileExt(map[string][]string{"Content-Type": {"text/event-stream; charset=utf-8"}}, false))
	require.Equal(t, ".txt", bodyFileExt(map[string][]string{"Content-Type": {"text/plain"}}, false))
	require.Equal(t, ".gz", bodyFileExt(map[string][]string{"Content-Type": {"application/json"}, "Content-Encoding": {"gzip"}}, false))
	require.Equal(t, ".json", bodyFileExt(map[string][]string{"Content-Type": {"application/json"}, "Content-Encoding": {"gzip"}}, true))
	require.Equal(t, ".bin", bodyFileExt(map[string][]string{"Content-Type": {"image/png"}}, false))
	require.Equal(t, ".bin", bodyFileExt(nil, false))

	s := newTestStore(t, nil)
	ref := writeReq(t, s, "dl-1", "sess-dl", time.Date(2026, 9, 5, 10, 0, 0, 0, time.Local), []byte(`{"a":"<b>"}`))
	// writeReq 的段头不带 Content-Type：补一个带 JSON 类型的请求验证文件名
	rec, err := s.Begin("dl-2", "sess-dl", "", time.Date(2026, 9, 5, 10, 0, 0, 0, time.Local))
	require.NoError(t, err)
	sec := rec.startSection(KindClientRequest, SectionHeader{Header: map[string][]string{"Content-Type": {"application/json"}}})
	sec.body([]byte(`{"x":1}`))
	sec.end(true, "", nil)
	rec.Finish()
	s.pending.Wait()

	r := apiRouter()
	w := apiGet(t, r, "/admin/audit-archive/body", url.Values{"ref": {rec.ArchiveRef()}, "kind": {KindClientRequest}, "index": {"0"}})
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Header().Get("Content-Disposition"), `dl-2-1-client_request-0.json"`)
	require.Equal(t, `{"x":1}`, w.Body.String())
	w = apiGet(t, r, "/admin/audit-archive/body", url.Values{"ref": {ref}, "kind": {KindClientRequest}, "index": {"0"}})
	require.Contains(t, w.Header().Get("Content-Disposition"), `client_request-0.bin"`)
}

func TestBundleDownloadIsZipWithRawArchive(t *testing.T) {
	s := newTestStore(t, nil)
	day := time.Date(2026, 9, 6, 10, 0, 0, 0, time.Local)
	ref := writeReq(t, s, "zip-1", "sess-zip", day, []byte(`{"hello":"zip"}`))
	r := apiRouter()

	unzip := func(b []byte) (string, []byte) {
		zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		require.NoError(t, err)
		require.Len(t, zr.File, 1, "zip 内只有一个文件：存档数据原样")
		f, err := zr.File[0].Open()
		require.NoError(t, err)
		defer func() { _ = f.Close() }()
		data, err := io.ReadAll(f)
		require.NoError(t, err)
		return zr.File[0].Name, data
	}

	w := apiGet(t, r, "/admin/audit-archive/bundle", url.Values{"ref": {ref}})
	require.Equal(t, 200, w.Code)
	require.Equal(t, "application/zip", w.Header().Get("Content-Type"))
	require.Contains(t, w.Header().Get("Content-Disposition"), `zip-1-1.zip"`)
	name, data := unzip(w.Body.Bytes())
	require.Equal(t, "zip-1-1.zst", name)
	file, err := os.ReadFile(s.activeRequestPath(ref))
	require.NoError(t, err)
	require.Equal(t, file, data, "active 中的请求：zip 内为存储文件原字节")

	// 归档后：zip 内为还原的单请求文件，解压内容与归档前一致
	ageActive(t, s, time.Now().Add(-96*time.Hour))
	_, err = s.ArchiveBefore(context.Background(), time.Now().Add(-72*time.Hour))
	require.NoError(t, err)
	w = apiGet(t, r, "/admin/audit-archive/bundle", url.Values{"ref": {ref}})
	require.Equal(t, 200, w.Code)
	_, archived := unzip(w.Body.Bytes())
	decode := func(b []byte) []byte {
		dec, err := newDecoder(bytes.NewReader(b))
		require.NoError(t, err)
		defer dec.Close()
		out, err := io.ReadAll(dec)
		require.NoError(t, err)
		return out
	}
	require.Equal(t, decode(file), decode(archived))

	w = apiGet(t, r, "/admin/audit-archive/bundle", url.Values{"ref": {"2026-09/06/sess-zip/none-1"}})
	require.Equal(t, 404, w.Code)
	require.Contains(t, w.Header().Get("Content-Type"), "application/json")
}
