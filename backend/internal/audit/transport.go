package audit

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
)

// AttemptInfo 是调用方已知的调度信息。
type AttemptInfo struct {
	AccountID int64
	// Proxy 仅记录是否经代理及代理方案，不记录凭据。
	Proxy string
	// Via 标识接入点（http_upstream / http_upstream_tls / openai_ws_dial）。
	Via string
}

// roundTripper 在真实传输之上旁路记录实际上游请求与上游响应：
// 记录的是交给 Transport 的最终请求（协议转换、UA 改写、默认头、账户切换之后），
// 以及 Transport 返回的原始响应字节（Content-Encoding 保持原样，展示时再解码）。
type roundTripper struct {
	base http.RoundTripper
	info AttemptInfo
}

// WrapClient 返回浅拷贝的 client，其 Transport 增加归档旁路。
// 没有归档器的请求（例如后台任务）原样透传。
func WrapClient(client *http.Client, info AttemptInfo) *http.Client {
	if client == nil || Default() == nil {
		return client
	}
	clone := *client
	base := clone.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	if rt, ok := base.(*roundTripper); ok {
		base = rt.base
	}
	clone.Transport = &roundTripper{base: base, info: info}
	return &clone
}

// RedactProxy 只保留代理方案与主机，去掉用户名密码。
func RedactProxy(proxy string) string {
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		return ""
	}
	if i := strings.Index(proxy, "@"); i >= 0 {
		scheme := ""
		if j := strings.Index(proxy, "://"); j >= 0 && j < i {
			scheme = proxy[:j+3]
		}
		return scheme + "***@" + proxy[i+1:]
	}
	return proxy
}

func (t *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := FromContext(req.Context())
	if rec == nil || rec.Finished() {
		return t.base.RoundTrip(req)
	}
	red := rec.store.red
	seq := rec.NextAttempt()
	spanID := newSpanID()
	transport := "http"
	if strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		transport = "websocket"
	}
	meta := map[string]any{
		"attempt":        seq,
		"account_id":     t.info.AccountID,
		"via":            t.info.Via,
		"proxy":          RedactProxy(t.info.Proxy),
		"trace_id":       rec.TraceID,
		"span_id":        spanID,
		"parent_span_id": rec.SpanID,
		"content_length": req.ContentLength,
	}
	// Transport 隐式行为：未显式声明 Accept-Encoding 时，net/http 会自动加 gzip 并透明解压。
	if req.Header.Get("Accept-Encoding") == "" && transport == "http" {
		meta["transport_implicit_accept_encoding"] = "gzip (net/http default, transparently decoded)"
	}
	host := req.Host
	if host == "" && req.URL != nil {
		host = req.URL.Host
	}
	reqSec := rec.startSection(KindUpstreamRequest, SectionHeader{
		Seq:       seq,
		Method:    req.Method,
		URL:       red.URL(req.URL),
		Proto:     req.Proto,
		Host:      host,
		Header:    red.Header(req.Header),
		Transport: transport,
		Meta:      meta,
	})

	out := req
	var tb *upstreamBody
	if req.Body != nil && req.Body != http.NoBody {
		out = req.Clone(req.Context())
		tb = &upstreamBody{ReadCloser: req.Body, sec: reqSec}
		out.Body = tb
		if req.GetBody != nil {
			orig := req.GetBody
			var replayMu sync.Mutex
			current := tb
			out.GetBody = func() (io.ReadCloser, error) {
				// Transport 重放请求体（连接复用失败 / 307 等）：前一段标记为未完成，开启新段。
				body, err := orig()
				if err != nil {
					return body, err
				}
				replayMu.Lock()
				defer replayMu.Unlock()
				current.replaced()
				sec := rec.startSection(KindUpstreamRequest, SectionHeader{Seq: seq, Replay: true, Method: req.Method, URL: red.URL(req.URL), Header: red.Header(req.Header), Transport: transport, Meta: map[string]any{"attempt": seq, "span_id": spanID}})
				current = &upstreamBody{ReadCloser: body, sec: sec}
				return current, nil
			}
		}
	} else {
		reqSec.end(true, "", nil)
	}

	resp, err := t.base.RoundTrip(out)
	if err != nil {
		rec.startSection(KindUpstreamResponse, SectionHeader{Seq: seq, Meta: map[string]any{"attempt": seq, "account_id": t.info.AccountID, "span_id": spanID}}).
			end(false, "transport error: "+err.Error(), nil)
		return resp, err
	}

	respMeta := map[string]any{
		"attempt":    seq,
		"account_id": t.info.AccountID,
		"span_id":    spanID,
	}
	if resp.Uncompressed {
		respMeta["transport_decoded"] = true
	}
	respTransport := "http"
	if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		respTransport = "sse"
	}
	if resp.StatusCode == http.StatusSwitchingProtocols {
		respTransport = "websocket"
	}
	respSec := rec.startSection(KindUpstreamResponse, SectionHeader{
		Seq:       seq,
		Status:    resp.StatusCode,
		Proto:     resp.Proto,
		Header:    red.Header(resp.Header),
		Transport: respTransport,
		Meta:      respMeta,
	})

	if resp.StatusCode == http.StatusSwitchingProtocols {
		// 升级后的连接由 WS 客户端以消息粒度记录（见 WSTap），这里不包装可写 Body。
		respSec.end(true, "", map[string]any{"upgraded": true})
		rec.mu.Lock()
		rec.lastWS = int32(seq) //nolint:gosec // bounded
		rec.mu.Unlock()
		return resp, nil
	}
	if resp.Body == nil || resp.Body == http.NoBody {
		respSec.end(true, "", nil)
		return resp, nil
	}
	resp.Body = &responseBody{ReadCloser: resp.Body, sec: respSec}
	return resp, nil
}

// upstreamBody 复制 Transport 实际读取并发送的请求体字节。
type upstreamBody struct {
	io.ReadCloser
	sec  *section
	mu   sync.Mutex
	eof  bool
	err  error
	done bool
}

func (b *upstreamBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.sec.body(p[:n])
	}
	if err != nil {
		b.mu.Lock()
		if errors.Is(err, io.EOF) {
			b.eof = true
		} else if b.err == nil {
			b.err = err
		}
		b.mu.Unlock()
		if errors.Is(err, io.EOF) {
			b.finish()
		}
	}
	return n, err
}

func (b *upstreamBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish()
	return err
}

func (b *upstreamBody) finish() {
	b.mu.Lock()
	if b.done {
		b.mu.Unlock()
		return
	}
	b.done = true
	eof, rerr := b.eof, b.err
	b.mu.Unlock()
	switch {
	case eof:
		b.sec.end(true, "", nil)
	case rerr != nil:
		b.sec.end(false, "read error: "+rerr.Error(), nil)
	default:
		b.sec.end(false, "request body closed before EOF", nil)
	}
}

func (b *upstreamBody) replaced() {
	b.mu.Lock()
	if b.done {
		b.mu.Unlock()
		return
	}
	b.done = true
	b.mu.Unlock()
	b.sec.end(false, "request body replayed by transport", nil)
}

// responseBody 复制业务实际读取到的上游响应字节。
type responseBody struct {
	io.ReadCloser
	sec  *section
	mu   sync.Mutex
	eof  bool
	err  error
	done bool
}

func (b *responseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.sec.body(p[:n])
	}
	if err != nil {
		b.mu.Lock()
		if errors.Is(err, io.EOF) {
			b.eof = true
		} else if b.err == nil {
			b.err = err
		}
		b.mu.Unlock()
		if errors.Is(err, io.EOF) {
			b.finish()
		}
	}
	return n, err
}

func (b *responseBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish()
	return err
}

func (b *responseBody) finish() {
	b.mu.Lock()
	if b.done {
		b.mu.Unlock()
		return
	}
	b.done = true
	eof, rerr := b.eof, b.err
	b.mu.Unlock()
	switch {
	case eof:
		b.sec.end(true, "", nil)
	case rerr != nil:
		b.sec.end(false, "read error: "+rerr.Error(), nil)
	default:
		b.sec.end(false, "response body closed before EOF (consumer stopped reading)", nil)
	}
}
