package audit

import (
	"encoding/binary"
	"time"
)

// 归档文件 = 一个完整的 zstd 流；解压后的内容为：
//
//	Magic(8) | Frame*
//	Frame = Type(1) | Seq(uint16 BE) | Len(uint32 BE) | Payload(Len)
//
// 一个文件内可以有多个 section（以 Seq 区分）：客户端两份文件只有 Seq=0，
// 上游两份文件中每个调度尝试（attempt）占一个 Seq（从 1 开始）。
// 正文以原始字节分块写入 FrameBody，不做任何改写或截断；
// section 以 FrameEnd 结束，缺失 FrameEnd 即视为不完整。
const (
	fileMagic = "S2AAUD01"

	FrameHeader byte = 'H' // JSON SectionHeader
	FrameBody   byte = 'B' // 正文原始字节
	FrameEnd    byte = 'E' // JSON SectionEnd
	FrameRaw    byte = 'R' // 协议升级（WebSocket 劫持）后的原始连接字节，展示时再解析帧
	FrameMsg    byte = 'W' // WebSocket 消息：opcode(1) | unixnano(8) | data
	FrameNote   byte = 'N' // JSON 附注（连接复用、用量回调等）

	frameHeaderSize = 7
	maxFramePayload = 1 << 20
)

// 四份文件的类型。
const (
	KindClientRequest    = "client_request"
	KindUpstreamRequest  = "upstream_request"
	KindUpstreamResponse = "upstream_response"
	KindClientResponse   = "client_response"
)

// Kinds 按展示顺序列出四份文件。
var Kinds = []string{KindClientRequest, KindUpstreamRequest, KindUpstreamResponse, KindClientResponse}

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

func encodeFrameHeader(dst []byte, typ byte, seq uint16, n int) []byte {
	var h [frameHeaderSize]byte
	h[0] = typ
	binary.BigEndian.PutUint16(h[1:3], seq)
	binary.BigEndian.PutUint32(h[3:7], uint32(n)) //nolint:gosec // n <= maxFramePayload
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
