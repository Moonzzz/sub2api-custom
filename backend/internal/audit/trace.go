package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// TraceInfo 是归档使用的 trace/span 关联信息。
type TraceInfo struct {
	TraceID      string `json:"trace_id"`
	SpanID       string `json:"span_id"`
	ParentSpanID string `json:"parent_span_id,omitempty"`
	// Source: otel_context（进程内已有 span）/ traceparent（客户端 W3C 头）/ generated
	Source string `json:"source"`
}

// traceFromRequest 优先复用 OTel 上下文中的 span，其次解析 W3C traceparent，
// 均无时生成新的 trace id。只读取，不向上游注入任何头，因此不改变真实调用。
func traceFromRequest(ctx context.Context, h http.Header) TraceInfo {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		return TraceInfo{TraceID: sc.TraceID().String(), ParentSpanID: sc.SpanID().String(), SpanID: newSpanID(), Source: "otel_context"}
	}
	extracted := propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(h))
	if sc := trace.SpanContextFromContext(extracted); sc.IsValid() {
		return TraceInfo{TraceID: sc.TraceID().String(), ParentSpanID: sc.SpanID().String(), SpanID: newSpanID(), Source: "traceparent"}
	}
	var tid [16]byte
	_, _ = rand.Read(tid[:])
	return TraceInfo{TraceID: hex.EncodeToString(tid[:]), SpanID: newSpanID(), Source: "generated"}
}

func newSpanID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
