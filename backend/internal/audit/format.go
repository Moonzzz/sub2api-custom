package audit

import (
	"encoding/binary"
	"time"
)

// 存储格式
//
// active（高速盘）：每个客户端请求一个文件 {active}/{yyyy-MM}/{dd}/{session}/{requestId}-{n}.zst，
// 内容是一个完整的 zstd 流，四个阶段（客户端请求 / 上游请求 / 上游响应 / 客户端响应）
// 共用这一条流压缩（客户端请求体与上游请求体高度相似，同流压缩可去重）：
//
//	RequestMagic(8) | Frame*
//
// archive（大容量盘）：每个会话一个文件 {archive}/{yyyy-MM}/{dd}/{session}.zst，
// 由夜间任务把会话内全部请求按时间顺序写入同一条 zstd 流（大窗口，跨请求去重）：
//
//	SessionMagic(8) | ( FrameEntry | <该请求的 Frame*> | FrameEntryEnd )*
//
// Frame = Type(1) | Kind(1) | Seq(uint16 BE) | Len(uint32 BE) | Payload(Len)
//
// Kind 区分四个阶段；Seq 区分同一阶段内的多个 section：客户端两个阶段只有 Seq=0，
// 上游两个阶段中每个调度尝试（attempt）占一个 Seq（从 1 开始）。
// 正文以原始字节分块写入 FrameBody，不做任何改写或截断；section 以 FrameEnd 结束，
// 缺失 FrameEnd 即视为不完整。
const (
	requestMagic = "S2AAUD02"
	sessionMagic = "S2AARC01"

	FrameHeader byte = 'H' // JSON SectionHeader
	FrameBody   byte = 'B' // 正文原始字节
	FrameEnd    byte = 'E' // JSON SectionEnd
	FrameRaw    byte = 'R' // 协议升级（WebSocket 劫持）后的原始连接字节，展示时再解析帧
	FrameMsg    byte = 'W' // WebSocket 消息：opcode(1) | unixnano(8) | data
	FrameNote   byte = 'N' // JSON 附注（连接复用、用量回调等）

	// 仅出现在会话归档文件中：一个请求的起止。
	FrameEntry    byte = 'Q' // JSON EntryHeader
	FrameEntryEnd byte = 'Z' // JSON EntryEnd

	frameHeaderSize = 8
	maxFramePayload = 1 << 20
)

// 四个阶段。
const (
	KindClientRequest    = "client_request"
	KindUpstreamRequest  = "upstream_request"
	KindUpstreamResponse = "upstream_response"
	KindClientResponse   = "client_response"
)

// Kinds 按展示顺序列出四个阶段；下标即帧头中的 Kind 字节。
var Kinds = []string{KindClientRequest, KindUpstreamRequest, KindUpstreamResponse, KindClientResponse}

// kindIndex 返回阶段的下标；未知阶段返回 -1。
func kindIndex(kind string) int {
	for i, k := range Kinds {
		if k == kind {
			return i
		}
	}
	return -1
}

// ValidKind 判断阶段名是否合法。
func ValidKind(kind string) bool { return kindIndex(kind) >= 0 }

// EntryHeader 是会话归档中一个请求条目的头。
type EntryHeader struct {
	Ref string `json:"ref"`
	// CompressedSize 为归档前 active 文件的压缩大小。
	CompressedSize int64     `json:"compressed_size"`
	ModTime        time.Time `json:"mtime"`
}

// EntryEnd 是会话归档中一个请求条目的尾。
type EntryEnd struct {
	// Status 为该请求在 active 中读取时的状态：ok / empty / truncated / corrupt / error。
	// 非 ok 时条目内保留了读取中断之前的全部完整帧。
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// SectionHeader 描述一段报文的起始信息（Header 已脱敏）。
type SectionHeader struct {
	Kind       string              `json:"kind"`
	Seq        int                 `json:"seq"`
	Time       time.Time           `json:"time"`
	Method     string              `json:"method,omitempty"`
	URL        string              `json:"url,omitempty"`
	Proto      string              `json:"proto,omitempty"`
	Host       string              `json:"host,omitempty"`
	RemoteAddr string              `json:"remote_addr,omitempty"`
	Status     int                 `json:"status,omitempty"`
	Header     map[string][]string `json:"header,omitempty"`
	// Transport: http / sse / websocket
	Transport string `json:"transport,omitempty"`
	// Replay 表示传输层（GetBody）重放了请求体，正文为新一轮发送。
	Replay bool           `json:"replay,omitempty"`
	Meta   map[string]any `json:"meta,omitempty"`
}

// SectionEnd 描述一段报文的结束信息。
type SectionEnd struct {
	Time  time.Time `json:"time"`
	Bytes int64     `json:"bytes"`
	// XXH64 是写入时对正文计算的 xxhash64（请求路径上的廉价完整性校验）；
	// 面向使用者的 SHA-256 在读取阶段计算。
	XXH64    string         `json:"xxh64"`
	Complete bool           `json:"complete"`
	Error    string         `json:"error,omitempty"`
	Meta     map[string]any `json:"meta,omitempty"`
	RawIn    int64          `json:"raw_in,omitempty"`
	Messages int64          `json:"messages,omitempty"`
}

func encodeFrameHeader(dst []byte, typ, kind byte, seq uint16, n int) []byte {
	var h [frameHeaderSize]byte
	h[0] = typ
	h[1] = kind
	binary.BigEndian.PutUint16(h[2:4], seq)
	binary.BigEndian.PutUint32(h[4:8], uint32(n)) //nolint:gosec // n <= maxFramePayload
	return append(dst, h[:]...)
}

// WebSocket 消息 opcode（与 RFC 6455 一致）。
const (
	WSOpText   byte = 1
	WSOpBinary byte = 2
	WSOpClose  byte = 8
	WSOpPing   byte = 9
	WSOpPong   byte = 10
)
