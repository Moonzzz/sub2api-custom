package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/cespare/xxhash/v2"
)

// ArchiveView 是一次客户端请求的完整归档视图（四个阶段 + 调度尝试汇总）。
type ArchiveView struct {
	Ref      string         `json:"ref"`
	Storage  StorageInfo    `json:"storage"`
	Files    []*FileView    `json:"files"`
	Summary  map[string]any `json:"summary"`
	Attempts []AttemptView  `json:"attempts"`
	// Status: complete / incomplete / in_progress / missing
	Status string   `json:"status"`
	Issues []string `json:"issues,omitempty"`
}

// AttemptView 汇总一次上游调度尝试。
type AttemptView struct {
	Seq        int    `json:"seq"`
	AccountID  any    `json:"account_id,omitempty"`
	Method     string `json:"method,omitempty"`
	URL        string `json:"url,omitempty"`
	Transport  string `json:"transport,omitempty"`
	Status     int    `json:"status,omitempty"`
	SpanID     any    `json:"span_id,omitempty"`
	Error      string `json:"error,omitempty"`
	Complete   bool   `json:"complete"`
	WSReused   bool   `json:"ws_reused,omitempty"`
	RequestIdx []int  `json:"request_sections"`
	RespIdx    []int  `json:"response_sections"`
}

// ReadArchive 读取并解析某个请求的四个阶段（先 active，后 archive）。
// 一次解压同时得到四个阶段，客户端请求体、上游请求体、响应体共用同一次读取。
func (s *Store) ReadArchive(ref string) (*ArchiveView, error) {
	if !ValidRef(ref) {
		return nil, errors.New("audit: invalid archive ref")
	}
	av := &ArchiveView{Ref: ref, Summary: map[string]any{}}
	limit := s.cfg.DisplayBodyLimit
	if limit <= 0 {
		limit = DefaultConfig().DisplayBodyLimit
	}
	files, info := s.readViews(ref, limit)
	av.Files, av.Storage = files, info
	byKind := map[string]*FileView{}
	for _, fv := range files {
		byKind[fv.Kind] = fv
	}
	if info.Status == "missing" {
		av.Status = "missing"
		return av, nil
	}
	if info.Status == "empty" {
		av.Status = "in_progress"
		av.Issues = append(av.Issues, "not yet flushed (request in progress, or process exited before flush)")
		return av, nil
	}
	s.postProcessWS(byKind)
	av.Attempts = buildAttempts(byKind[KindUpstreamRequest], byKind[KindUpstreamResponse])

	// 汇总与完整性判断
	if cr := byKind[KindClientRequest]; len(cr.Sections) > 0 {
		h := cr.Sections[0].Header
		av.Summary["method"] = h.Method
		av.Summary["url"] = h.URL
		av.Summary["transport"] = h.Transport
		av.Summary["started_at"] = h.Time
		for k, v := range h.Meta {
			av.Summary[k] = v
		}
	}
	if cw := byKind[KindClientResponse]; len(cw.Sections) > 0 {
		sec := cw.Sections[0]
		av.Summary["client_status"] = sec.Header.Status
		if sec.End != nil {
			for k, v := range sec.End.Meta {
				av.Summary[k] = v
			}
			av.Summary["finished_at"] = sec.End.Time
		}
		av.Summary["usage_notes"] = cw.Notes
	}
	av.Status = "complete"
	if info.Status != "ok" {
		av.Status = "incomplete"
		av.Issues = append(av.Issues, fmt.Sprintf("%s: %s %s", info.Location, info.Status, info.Error))
	}
	for _, fv := range av.Files {
		for _, sec := range fv.Sections {
			if sec.End == nil {
				if av.Status == "complete" {
					av.Status = "incomplete"
				}
				av.Issues = append(av.Issues, fmt.Sprintf("%s#%d: section has no end marker", fv.Kind, sec.Index))
			} else if !sec.End.Complete {
				av.Issues = append(av.Issues, fmt.Sprintf("%s#%d: %s", fv.Kind, sec.Index, sec.End.Error))
			}
			if sec.HashMatch != nil && !*sec.HashMatch {
				av.Status = "incomplete"
				av.Issues = append(av.Issues, fmt.Sprintf("%s#%d: body hash mismatch", fv.Kind, sec.Index))
			}
		}
	}
	if av.Status == "complete" {
		if cw := byKind[KindClientResponse]; len(cw.Sections) == 0 || cw.Sections[0].End == nil || !cw.Sections[0].End.Complete {
			av.Status = "incomplete"
		}
	}
	return av, nil
}

// postProcessWS 把客户端侧劫持连接的原始字节解析为 WebSocket 消息。
func (s *Store) postProcessWS(byKind map[string]*FileView) {
	cr, cw := byKind[KindClientRequest], byKind[KindClientResponse]
	ext := ""
	if len(cw.Sections) > 0 {
		ext = headerValue(cw.Sections[0].Header.Header, "Sec-WebSocket-Extensions")
	}
	defl := parseWSExtensions(ext)
	for _, sec := range cr.Sections {
		if len(sec.rawConn) > 0 {
			sec.Messages, sec.MessagesTruncated = parseWSFrames(sec.rawConn, "client→gateway", defl, true, 5000)
			sec.MessagesTruncated = sec.MessagesTruncated || sec.overflow
		}
		sec.rawConn = nil
	}
	for _, sec := range cw.Sections {
		if len(sec.rawConn) > 0 {
			sec.Messages, sec.MessagesTruncated = parseWSFrames(sec.rawConn, "gateway→client", defl, false, 5000)
			sec.MessagesTruncated = sec.MessagesTruncated || sec.overflow
		}
		sec.rawConn = nil
	}
	for _, kind := range []string{KindUpstreamRequest, KindUpstreamResponse} {
		dir := "gateway→upstream"
		if kind == KindUpstreamResponse {
			dir = "upstream→gateway"
		}
		for _, sec := range byKind[kind].Sections {
			for i := range sec.Messages {
				sec.Messages[i].Dir = dir
			}
			sec.rawConn = nil
		}
	}
}

func buildAttempts(req, resp *FileView) []AttemptView {
	m := map[int]*AttemptView{}
	get := func(seq int) *AttemptView {
		if a, ok := m[seq]; ok {
			return a
		}
		a := &AttemptView{Seq: seq, Complete: true, RequestIdx: []int{}, RespIdx: []int{}}
		m[seq] = a
		return a
	}
	for _, sec := range req.Sections {
		a := get(sec.Seq)
		a.RequestIdx = append(a.RequestIdx, sec.Index)
		if sec.Header.Method != "" && a.Method == "" {
			a.Method, a.URL = sec.Header.Method, sec.Header.URL
			a.AccountID = sec.Header.Meta["account_id"]
			a.SpanID = sec.Header.Meta["span_id"]
		}
		if a.Transport == "" {
			a.Transport = sec.Header.Transport
		}
		if v, ok := sec.Header.Meta["ws_reused"].(bool); ok && v {
			a.WSReused = true
		}
		if sec.End == nil || !sec.End.Complete {
			a.Complete = false
		}
	}
	for _, sec := range resp.Sections {
		a := get(sec.Seq)
		a.RespIdx = append(a.RespIdx, sec.Index)
		if sec.Header.Status != 0 && a.Status == 0 {
			a.Status = sec.Header.Status
		}
		if a.AccountID == nil {
			a.AccountID = sec.Header.Meta["account_id"]
		}
		if sec.End == nil || !sec.End.Complete {
			a.Complete = false
			if sec.End != nil && a.Error == "" {
				a.Error = sec.End.Error
			}
		}
	}
	out := make([]AttemptView, 0, len(m))
	for _, a := range m {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// BodyInfo 是下载前预扫描得到的段信息。SHA256 / Bytes 为读取时对原始正文的计算值，
// Intact 表示与写入时记录的字节数、xxhash64 一致。
type BodyInfo struct {
	Header SectionHeader
	End    *SectionEnd
	SHA256 string
	Bytes  int64
	Intact bool
}

// StreamBody 把某段正文原样（或按 Content-Encoding 解码后）流式写出，
// 全程在内存中解压，不落临时文件。返回写出的字节数。
func (s *Store) StreamBody(w io.Writer, ref, kind string, index int, decode bool, onHeader func(BodyInfo)) (int64, error) {
	if !ValidRef(ref) {
		return 0, errors.New("audit: invalid archive ref")
	}
	ki := kindIndex(kind)
	if ki < 0 {
		return 0, errors.New("audit: invalid kind")
	}
	// 先扫描一遍取段头/段尾并计算 SHA-256（用于响应头），再流式输出正文。
	info, err := s.sectionMeta(ref, byte(ki), index)
	if err != nil {
		return 0, err
	}
	hdr := info.Header
	if onHeader != nil {
		onHeader(info)
	}
	enc := headerValue(hdr.Header, "Content-Encoding")
	if !decode || enc == "" || enc == "identity" {
		return s.copySection(w, ref, byte(ki), index)
	}
	pr, pw := io.Pipe()
	go func() {
		_, err := s.copySection(pw, ref, byte(ki), index)
		_ = pw.CloseWithError(err)
	}()
	rc, err := DecodeReader(enc, pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		return 0, err
	}
	defer func() { _ = rc.Close(); _ = pr.Close() }()
	return io.Copy(w, rc)
}

// walkSections 遍历请求中某个阶段的帧，idx 为该阶段内的 section 序号。
func (s *Store) walkSections(ref string, kind byte, fn func(idx int, f frame) bool) error {
	latest := map[uint16]int{}
	count := 0
	_, err := s.walkRequest(ref, func(fx frame) bool {
		if fx.kind != kind {
			return true
		}
		idx := -1
		if fx.typ == FrameHeader {
			idx = count
			latest[fx.seq] = count
			count++
		} else if v, ok := latest[fx.seq]; ok {
			idx = v
		}
		if idx >= 0 {
			return fn(idx, fx)
		}
		return true
	})
	return err
}

func (s *Store) sectionMeta(ref string, kind byte, index int) (BodyInfo, error) {
	var hdr SectionHeader
	var end *SectionEnd
	found := false
	sh, xh := sha256.New(), xxhash.New()
	var n int64
	err := s.walkSections(ref, kind, func(idx int, f frame) bool {
		if idx != index {
			return true
		}
		switch f.typ {
		case FrameHeader:
			found = true
			_ = jsonUnmarshal(f.payload, &hdr)
		case FrameBody:
			_, _ = sh.Write(f.payload)
			_, _ = xh.Write(f.payload)
			n += int64(len(f.payload))
		case FrameEnd:
			var e SectionEnd
			if jsonUnmarshal(f.payload, &e) == nil {
				end = &e
			}
			return false
		}
		return true
	})
	if err != nil && !found {
		return BodyInfo{}, err
	}
	if !found {
		return BodyInfo{}, fmt.Errorf("%w: %d", errSectionNotFound, index)
	}
	info := BodyInfo{Header: hdr, End: end, SHA256: hex.EncodeToString(sh.Sum(nil)), Bytes: n}
	info.Intact = end != nil && end.Bytes == n && end.XXH64 == xxh64Hex(xh.Sum64())
	return info, nil
}

func (s *Store) copySection(w io.Writer, ref string, kind byte, index int) (int64, error) {
	var n int64
	var werr error
	err := s.walkSections(ref, kind, func(idx int, f frame) bool {
		if idx != index {
			return true
		}
		if f.typ == FrameEnd {
			return false
		}
		if f.typ != FrameBody {
			return true
		}
		m, err := w.Write(f.payload)
		n += int64(m)
		if err != nil {
			werr = err
			return false
		}
		return true
	})
	if werr != nil {
		return n, werr
	}
	return n, err
}

var jsonUnmarshal = json.Unmarshal
