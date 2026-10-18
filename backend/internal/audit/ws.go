package audit

import (
	"context"
	"sync"
)

// WSTap 以消息粒度记录上游 WebSocket 连接的收发（已解除 permessage-deflate）。
//
// 上游 WS 连接可能被连接池复用、由常驻读循环以后台 context 读取，因此：
//   - 带归档器的 context 调用时，连接绑定到该请求（若与上次不同则切换并开启新段）；
//   - 不带归档器的 context（读循环）沿用最近绑定的请求。
//
// 握手请求/响应头由 RoundTripper 在建连时记录；复用连接在新请求中以
// ws_reused 段标注原建连请求。
type WSTap struct {
	mu      sync.Mutex
	rec     *Recorder
	seq     int
	out     *section
	in      *section
	dialReq string
	dialSeq int
}

// NewWSTap 在建连成功后创建 Tap；ctx 中无归档器时仍返回可用的 Tap（等待后续绑定）。
func NewWSTap(ctx context.Context) *WSTap {
	if Default() == nil {
		return nil
	}
	t := &WSTap{}
	if rec := FromContext(ctx); rec != nil {
		rec.mu.Lock()
		t.dialSeq = int(rec.lastWS)
		rec.lastWS = 0
		rec.mu.Unlock()
		t.dialReq = rec.ID
		t.bindLocked(rec, t.dialSeq, false)
	}
	return t
}

func (t *WSTap) bindLocked(rec *Recorder, seq int, reused bool) {
	if t.rec != nil {
		t.out.end(true, "", map[string]any{"ws_released": true})
		t.in.end(true, "", map[string]any{"ws_released": true})
	}
	t.rec, t.out, t.in = rec, nil, nil
	if rec == nil || rec.Finished() {
		t.rec = nil
		return
	}
	if seq <= 0 {
		seq = rec.NextAttempt()
	}
	t.seq = seq
	meta := map[string]any{"attempt": seq, "ws_messages": true}
	if reused {
		meta["ws_reused"] = true
		meta["dial_request_id"] = t.dialReq
	}
	t.out = rec.startSection(KindUpstreamRequest, SectionHeader{Seq: seq, Transport: "websocket", Meta: meta})
	t.in = rec.startSection(KindUpstreamResponse, SectionHeader{Seq: seq, Transport: "websocket", Meta: meta})
}

func (t *WSTap) target(ctx context.Context, outbound bool) *section {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if rec := FromContext(ctx); rec != nil && rec != t.rec && !rec.Finished() {
		t.bindLocked(rec, 0, true)
	}
	if t.rec == nil || t.rec.Finished() {
		return nil
	}
	if outbound {
		return t.out
	}
	return t.in
}

// Sent 记录发往上游的一条消息。
func (t *WSTap) Sent(ctx context.Context, op byte, data []byte) {
	if s := t.target(ctx, true); s != nil {
		s.message(op, data)
	}
}

// Received 记录从上游收到的一条消息。
func (t *WSTap) Received(ctx context.Context, op byte, data []byte) {
	if s := t.target(ctx, false); s != nil {
		s.message(op, data)
	}
}

// Closed 记录连接关闭（错误原因写入两段的结束信息）。
func (t *WSTap) Closed(reason string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.rec == nil {
		return
	}
	complete := reason == ""
	t.out.end(complete, reason, nil)
	t.in.end(complete, reason, nil)
	t.rec = nil
}
