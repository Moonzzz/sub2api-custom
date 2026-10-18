package audit

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/andybalholm/brotli"
	"github.com/cespare/xxhash/v2"
	"github.com/klauspost/compress/zstd"
)

// ErrNotArchive 表示文件不是本归档格式。
var ErrNotArchive = errors.New("audit: not an archive file")

// newDecoder 在内存中流式解压，不产生临时文件。
func newDecoder(r io.Reader) (*zstd.Decoder, error) {
	return zstd.NewReader(r,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxWindow(64<<20),
	)
}

// frameReader 顺序读取解压后的帧。
type frameReader struct {
	r   *bufio.Reader
	hdr [frameHeaderSize]byte
}

type frame struct {
	typ     byte
	seq     uint16
	payload []byte
}

func openFrames(r io.Reader) (*frameReader, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	magic := make([]byte, len(fileMagic))
	if _, err := io.ReadFull(br, magic); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, err
	}
	if string(magic) != fileMagic {
		return nil, ErrNotArchive
	}
	return &frameReader{r: br}, nil
}

// next 读取一帧；payload 复用 buf。返回 io.EOF 表示正常结束，io.ErrUnexpectedEOF 表示截断。
func (fr *frameReader) next(buf []byte) (frame, error) {
	if _, err := io.ReadFull(fr.r, fr.hdr[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return frame{}, io.EOF
		}
		return frame{}, io.ErrUnexpectedEOF
	}
	n := int(binary.BigEndian.Uint32(fr.hdr[3:7]))
	if n > maxFramePayload+16 {
		return frame{}, fmt.Errorf("audit: frame too large (%d)", n)
	}
	if cap(buf) < n {
		buf = make([]byte, n)
	}
	buf = buf[:n]
	if _, err := io.ReadFull(fr.r, buf); err != nil {
		return frame{}, io.ErrUnexpectedEOF
	}
	return frame{typ: fr.hdr[0], seq: binary.BigEndian.Uint16(fr.hdr[1:3]), payload: buf}, nil
}

// FileView 是单个归档文件的展示视图。
type FileView struct {
	Kind           string           `json:"kind"`
	Exists         bool             `json:"exists"`
	CompressedSize int64            `json:"compressed_size"`
	RawSize        int64            `json:"raw_size"`
	Status         string           `json:"status"` // ok / missing / empty / truncated / corrupt
	Error          string           `json:"error,omitempty"`
	Sections       []*SectionView   `json:"sections"`
	Notes          []map[string]any `json:"notes,omitempty"`
}

// SectionView 是一段报文的展示视图。
type SectionView struct {
	Index    int           `json:"index"`
	Seq      int           `json:"seq"`
	Header   SectionHeader `json:"header"`
	End      *SectionEnd   `json:"end,omitempty"`
	Complete bool          `json:"complete"`
	// BodyBytes / BodySHA256 为读取时计算的值；HashMatch 以写入时记录的字节数与 xxhash64 比对。
	BodyBytes         int64       `json:"body_bytes"`
	BodySHA256        string      `json:"body_sha256"`
	HashMatch         *bool       `json:"hash_match,omitempty"`
	Body              *BodyView   `json:"body,omitempty"`
	Messages          []WSMessage `json:"messages,omitempty"`
	MessagesTruncated bool        `json:"messages_truncated,omitempty"`
	RawBytes          int64       `json:"raw_bytes,omitempty"`

	hash     hash.Hash
	xxh      *xxhash.Digest
	rawBody  []byte
	rawConn  []byte
	limit    int
	overflow bool
}

// BodyView 是正文的展示形态（仅在展示阶段解析）。
type BodyView struct {
	ContentEncoding string     `json:"content_encoding,omitempty"`
	Decoded         bool       `json:"decoded"`
	DecodeError     string     `json:"decode_error,omitempty"`
	IsText          bool       `json:"is_text"`
	Text            string     `json:"text,omitempty"`
	Base64          []byte     `json:"base64,omitempty"`
	TruncatedForUI  bool       `json:"truncated_for_display"`
	SSE             []SSEEvent `json:"sse,omitempty"`
	SSEDone         *bool      `json:"sse_terminated,omitempty"`
}

// SSEEvent 是解析后的一个 SSE 事件。
type SSEEvent struct {
	Event string `json:"event,omitempty"`
	ID    string `json:"id,omitempty"`
	Data  string `json:"data"`
}

// WSMessage 是一条 WebSocket 消息。
type WSMessage struct {
	Dir        string    `json:"dir"` // in / out（相对写入方）
	Op         string    `json:"op"`
	Time       time.Time `json:"time,omitempty"`
	Size       int       `json:"size"`
	Compressed bool      `json:"compressed,omitempty"`
	Text       string    `json:"text,omitempty"`
	Base64     []byte    `json:"base64,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// ReadFile 解析一个归档文件。displayLimit 限制每段保留用于展示的正文字节，
// 超出部分仍参与计数与哈希校验，可通过下载接口完整获取。
func ReadFile(path, kind string, displayLimit int) *FileView {
	fv := &FileView{Kind: kind, Sections: []*SectionView{}}
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		fv.Status = "missing"
		return fv
	}
	if err != nil {
		fv.Status, fv.Error = "error", err.Error()
		return fv
	}
	fv.Exists = true
	fv.CompressedSize = st.Size()
	if st.Size() == 0 {
		// 文件已创建但尚未落盘：请求仍在进行，或进程在落盘前退出。
		fv.Status = "empty"
		return fv
	}
	f, err := os.Open(path) //nolint:gosec // 路径已由 FilePath 校验
	if err != nil {
		fv.Status, fv.Error = "error", err.Error()
		return fv
	}
	defer func() { _ = f.Close() }()
	dec, err := newDecoder(f)
	if err != nil {
		fv.Status, fv.Error = "corrupt", err.Error()
		return fv
	}
	defer dec.Close()
	counter := &countingReader{r: dec}
	fr, err := openFrames(counter)
	if err != nil {
		fv.Status, fv.Error = "corrupt", err.Error()
		return fv
	}
	latest := map[uint16]*SectionView{}
	var buf []byte
	fv.Status = "ok"
	for {
		fr2, err := fr.next(buf)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fv.Status, fv.Error = "truncated", err.Error()
			break
		}
		buf = fr2.payload[:0]
		switch fr2.typ {
		case FrameHeader:
			sv := &SectionView{Index: len(fv.Sections), Seq: int(fr2.seq), hash: sha256.New(), xxh: xxhash.New(), limit: displayLimit}
			if err := json.Unmarshal(fr2.payload, &sv.Header); err != nil {
				sv.Header.Kind = kind
			}
			fv.Sections = append(fv.Sections, sv)
			latest[fr2.seq] = sv
		case FrameNote:
			var note map[string]any
			if json.Unmarshal(fr2.payload, &note) == nil {
				fv.Notes = append(fv.Notes, note)
			}
		default:
			sv := latest[fr2.seq]
			if sv == nil {
				continue
			}
			sv.consume(fr2)
		}
	}
	fv.RawSize = counter.n
	if fv.Status == "ok" && counter.err != nil && !errors.Is(counter.err, io.EOF) {
		fv.Status, fv.Error = "truncated", counter.err.Error()
	}
	for _, sv := range fv.Sections {
		sv.finalize()
	}
	return fv
}

type countingReader struct {
	r   io.Reader
	n   int64
	err error
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if err != nil {
		c.err = err
	}
	return n, err
}

func (sv *SectionView) consume(f frame) {
	switch f.typ {
	case FrameBody:
		_, _ = sv.hash.Write(f.payload)
		_, _ = sv.xxh.Write(f.payload)
		sv.BodyBytes += int64(len(f.payload))
		if room := sv.limit - len(sv.rawBody); room > 0 {
			n := len(f.payload)
			if n > room {
				n = room
				sv.overflow = true
			}
			sv.rawBody = append(sv.rawBody, f.payload[:n]...)
		} else {
			sv.overflow = true
		}
	case FrameRaw:
		sv.RawBytes += int64(len(f.payload))
		if room := sv.limit - len(sv.rawConn); room > 0 {
			n := min(len(f.payload), room)
			sv.rawConn = append(sv.rawConn, f.payload[:n]...)
			if n < len(f.payload) {
				sv.overflow = true
			}
		} else {
			sv.overflow = true
		}
	case FrameMsg:
		if len(f.payload) < 9 {
			return
		}
		if len(sv.Messages) >= 5000 {
			sv.MessagesTruncated = true
			return
		}
		data := f.payload[9:]
		m := WSMessage{
			Op:   opName(f.payload[0]),
			Time: time.Unix(0, int64(binary.BigEndian.Uint64(f.payload[1:9]))), //nolint:gosec // 写入时为正数
			Size: len(data),
		}
		setPayload(&m.Text, &m.Base64, data, 256<<10)
		sv.Messages = append(sv.Messages, m)
	case FrameEnd:
		var e SectionEnd
		if json.Unmarshal(f.payload, &e) == nil {
			sv.End = &e
		}
	}
}

func (sv *SectionView) finalize() {
	sv.BodySHA256 = hex.EncodeToString(sv.hash.Sum(nil))
	if sv.End != nil {
		sv.Complete = sv.End.Complete
		match := sv.End.Bytes == sv.BodyBytes && sv.End.XXH64 == xxh64Hex(sv.xxh.Sum64())
		sv.HashMatch = &match
	}
	if sv.BodyBytes > 0 {
		sv.Body = buildBodyView(sv.Header, sv.rawBody, sv.overflow)
	}
	sv.rawBody = nil
}

func setPayload(text *string, b64 *[]byte, data []byte, limit int) {
	if len(data) > limit {
		data = data[:limit]
	}
	if utf8.Valid(data) {
		*text = string(data)
	} else {
		*b64 = append([]byte(nil), data...)
	}
}

func opName(op byte) string {
	switch op {
	case WSOpText:
		return "text"
	case WSOpBinary:
		return "binary"
	case WSOpClose:
		return "close"
	case WSOpPing:
		return "ping"
	case WSOpPong:
		return "pong"
	case 0:
		return "continuation"
	default:
		return fmt.Sprintf("op_%d", op)
	}
}

func headerValue(h map[string][]string, key string) string {
	for k, vs := range h {
		if strings.EqualFold(k, key) && len(vs) > 0 {
			return vs[0]
		}
	}
	return ""
}

// buildBodyView 解码 Content-Encoding（仅展示用途），并解析 SSE。
func buildBodyView(h SectionHeader, raw []byte, overflow bool) *BodyView {
	bv := &BodyView{TruncatedForUI: overflow}
	enc := strings.ToLower(strings.TrimSpace(headerValue(h.Header, "Content-Encoding")))
	data := raw
	if enc != "" && enc != "identity" {
		bv.ContentEncoding = enc
		decoded, err := decodeContent(enc, raw, 4*len(raw)+(1<<20))
		if err != nil && len(decoded) == 0 {
			bv.DecodeError = err.Error()
		} else {
			bv.Decoded = true
			if err != nil {
				bv.DecodeError = "partial: " + err.Error()
			}
			data = decoded
		}
	}
	if utf8.Valid(data) || (overflow && utf8.Valid(trimIncompleteRune(data))) {
		bv.IsText = true
		bv.Text = string(data)
	} else {
		bv.Base64 = data
	}
	ct := strings.ToLower(headerValue(h.Header, "Content-Type"))
	if bv.IsText && (strings.HasPrefix(ct, "text/event-stream") || h.Transport == "sse") {
		bv.SSE, bv.SSEDone = parseSSE(bv.Text)
	}
	return bv
}

func trimIncompleteRune(b []byte) []byte {
	for i := 0; i < 4 && len(b) > 0; i++ {
		if utf8.Valid(b) {
			return b
		}
		b = b[:len(b)-1]
	}
	return b
}

// DecodeReader 返回 Content-Encoding 解码读取器（内存流式）。
func DecodeReader(enc string, r io.Reader) (io.ReadCloser, error) {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "gzip", "x-gzip":
		return gzip.NewReader(r)
	case "deflate":
		return flate.NewReader(r), nil
	case "br":
		return io.NopCloser(brotli.NewReader(r)), nil
	case "zstd":
		d, err := newDecoder(r)
		if err != nil {
			return nil, err
		}
		return d.IOReadCloser(), nil
	default:
		return nil, fmt.Errorf("unsupported content-encoding %q", enc)
	}
}

func decodeContent(enc string, raw []byte, limit int) ([]byte, error) {
	rc, err := DecodeReader(enc, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	out, err := io.ReadAll(io.LimitReader(rc, int64(limit)))
	return out, err
}

func parseSSE(text string) ([]SSEEvent, *bool) {
	var events []SSEEvent
	done := false
	text = strings.ReplaceAll(text, "\r\n", "\n")
	for _, block := range strings.Split(text, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var ev SSEEvent
		var data []string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event:"):
				ev.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "id:"):
				ev.ID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			case strings.HasPrefix(line, "data:"):
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		ev.Data = strings.Join(data, "\n")
		if ev.Data == "[DONE]" || ev.Event == "message_stop" || ev.Event == "response.completed" || ev.Event == "response.done" ||
			strings.Contains(ev.Data, `"finishReason"`) {
			done = true
		}
		events = append(events, ev)
		if len(events) >= 20000 {
			break
		}
	}
	return events, &done
}
