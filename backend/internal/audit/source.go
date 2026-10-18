package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/klauspost/compress/zstd"
)

// 存储位置。
const (
	LocationActive  = "active"
	LocationArchive = "archive"
	LocationMissing = "missing"
)

// errRequestNotFound 表示 active 与 archive 中都没有该请求。
var errRequestNotFound = errors.New("audit: request not found")

// StorageInfo 描述一个请求当前的存储位置与读取状态。
type StorageInfo struct {
	// Location: active / archive / missing
	Location string `json:"location"`
	// Status: ok / empty / truncated / corrupt / error / missing
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	// CompressedSize：active 为该请求文件的大小；archive 为归档前 active 文件的大小。
	CompressedSize int64 `json:"compressed_size"`
	// SessionCompressedSize：archive 中整个会话文件的大小（会话内所有请求共用）。
	SessionCompressedSize int64 `json:"session_compressed_size,omitempty"`
	// RawSize：该请求解压后的帧字节数。
	RawSize int64 `json:"raw_size"`
}

// walkRequest 按 active → archive 的顺序定位请求，并依次把该请求的帧交给 fn
// （fn 返回 false 提前结束）。全程流式解压，内存只占解压窗口；
// 在 archive 中只解压到目标请求为止，其余请求的负载被跳过而不复制。
//
// 返回的 error 仅表示“读取中断”（截断、损坏、I/O 错误），info.Status 同步记录；
// 请求不存在时返回 errRequestNotFound。
func (s *Store) walkRequest(ref string, fn func(f frame) bool) (StorageInfo, error) {
	if !ValidRef(ref) {
		return StorageInfo{Location: LocationMissing, Status: "missing"}, errors.New("audit: invalid archive ref")
	}
	info, err := s.walkActive(ref, fn)
	if !errors.Is(err, errRequestNotFound) {
		return info, err
	}
	return s.walkArchive(ref, fn)
}

func (s *Store) walkActive(ref string, fn func(f frame) bool) (StorageInfo, error) {
	info := StorageInfo{Location: LocationActive, Status: "ok"}
	f, err := os.Open(s.activeRequestPath(ref)) //nolint:gosec // 路径由已校验的引用派生
	if errors.Is(err, fs.ErrNotExist) {
		return StorageInfo{Location: LocationMissing, Status: "missing"}, errRequestNotFound
	}
	if err != nil {
		return info.fail("error", err)
	}
	defer func() { _ = f.Close() }()
	if st, err := f.Stat(); err == nil {
		info.CompressedSize = st.Size()
		if st.Size() == 0 {
			// 文件已创建但尚未落盘：请求仍在进行，或进程在落盘前退出。
			info.Status = "empty"
			return info, nil
		}
	}
	dec, err := newDecoder(f)
	if err != nil {
		return info.fail("corrupt", err)
	}
	defer dec.Close()
	fr, err := openFrames(dec, requestMagic)
	if err != nil {
		return info.fail("corrupt", err)
	}
	info.RawSize = int64(len(requestMagic))
	var buf []byte
	for {
		fx, err := fr.next(buf)
		if errors.Is(err, io.EOF) {
			return info, nil
		}
		if err != nil {
			return info.fail("truncated", err)
		}
		buf = fx.payload[:0]
		info.RawSize += int64(frameHeaderSize + len(fx.payload))
		if !fn(fx) {
			return info, nil
		}
	}
}

// archiveReadSlots 限制同时解压会话归档的数量（每个约占 2 倍归档窗口的内存）。
const archiveReadSlots = 4

func (s *Store) walkArchive(ref string, fn func(f frame) bool) (StorageInfo, error) {
	missing := StorageInfo{Location: LocationMissing, Status: "missing"}
	s.archiveReads <- struct{}{}
	defer func() { <-s.archiveReads }()
	f, err := os.Open(s.archiveSessionPath(refSessionDir(ref))) //nolint:gosec // 路径由已校验的引用派生
	if errors.Is(err, fs.ErrNotExist) {
		return missing, errRequestNotFound
	}
	info := StorageInfo{Location: LocationArchive, Status: "ok"}
	if err != nil {
		return info.fail("error", err)
	}
	defer func() { _ = f.Close() }()
	if st, err := f.Stat(); err == nil {
		info.SessionCompressedSize = st.Size()
	}
	dec, err := newDecoder(f)
	if err != nil {
		return info.fail("corrupt", err)
	}
	defer dec.Close()
	fr, err := openFrames(dec, sessionMagic)
	if err != nil {
		return info.fail("corrupt", err)
	}
	found := false
	var buf []byte
	for {
		typ, kind, seq, n, err := fr.header()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if found {
				return info.fail("truncated", err)
			}
			return info.fail("corrupt", err)
		}
		switch {
		case typ == FrameEntry:
			if found {
				return info, nil // 下一个条目开始：目标条目结束
			}
			p, err := fr.payload(buf, n)
			if err != nil {
				return info.fail("corrupt", err)
			}
			buf = p[:0]
			var eh EntryHeader
			if json.Unmarshal(p, &eh) == nil && eh.Ref == ref {
				found = true
				info.CompressedSize = eh.CompressedSize
				info.RawSize = int64(len(requestMagic))
			}
		case typ == FrameEntryEnd:
			if !found {
				if err := fr.skip(n); err != nil {
					return info.fail("corrupt", err)
				}
				continue
			}
			p, err := fr.payload(buf, n)
			if err != nil {
				return info.fail("truncated", err)
			}
			var ee EntryEnd
			if json.Unmarshal(p, &ee) == nil && ee.Status != "" && ee.Status != "ok" {
				info.Status, info.Error = ee.Status, ee.Error
			}
			return info, nil
		case found:
			p, err := fr.payload(buf, n)
			if err != nil {
				return info.fail("truncated", err)
			}
			buf = p[:0]
			info.RawSize += int64(frameHeaderSize + n)
			if !fn(frame{typ: typ, kind: kind, seq: seq, payload: p}) {
				return info, nil
			}
		default:
			if err := fr.skip(n); err != nil {
				return info.fail("corrupt", err)
			}
		}
	}
	if !found {
		return missing, errRequestNotFound
	}
	// 条目没有结束帧（归档文件被截断）。
	return info.fail("truncated", io.ErrUnexpectedEOF)
}

func (i StorageInfo) fail(status string, err error) (StorageInfo, error) {
	if i.Status == "ok" || i.Status == "" {
		i.Status = status
	}
	i.Error = err.Error()
	return i, err
}

// WriteRequestBundle 把一个请求的全部阶段作为单个 zstd 流写出（与 active 文件格式相同：
// RequestMagic | Frame*），四个阶段共用一次压缩，适合整体下载传输。
// 请求在 active 中时直接复制文件字节，不重新压缩；在 archive 中时只解压该请求并重新压缩。
func (s *Store) WriteRequestBundle(w io.Writer, ref string) (StorageInfo, error) {
	if !ValidRef(ref) {
		return StorageInfo{Location: LocationMissing, Status: "missing"}, errors.New("audit: invalid archive ref")
	}
	if f, err := os.Open(s.activeRequestPath(ref)); err == nil { //nolint:gosec // 路径由已校验的引用派生
		defer func() { _ = f.Close() }()
		n, err := io.Copy(w, f)
		return StorageInfo{Location: LocationActive, Status: "ok", CompressedSize: n}, err
	} else if !errors.Is(err, fs.ErrNotExist) {
		return StorageInfo{Location: LocationActive, Status: "error", Error: err.Error()}, err
	}
	// 编码器在找到目标请求后才创建：请求不存在时不向 w 写任何字节，调用方仍可返回 404。
	var enc *zstd.Encoder
	var werr error
	start := func() bool {
		if enc != nil {
			return true
		}
		enc, werr = zstd.NewWriter(w, encoderOptions(s.cfg.EncoderLevel, s.cfg.ActiveWindowMB, true)...)
		if werr == nil {
			_, werr = enc.Write([]byte(requestMagic))
		}
		return werr == nil
	}
	hdr := make([]byte, 0, frameHeaderSize)
	info, err := s.walkArchive(ref, func(f frame) bool {
		if !start() {
			return false
		}
		hdr = encodeFrameHeader(hdr[:0], f.typ, f.kind, f.seq, len(f.payload))
		if _, werr = enc.Write(hdr); werr == nil {
			_, werr = enc.Write(f.payload)
		}
		return werr == nil
	})
	if errors.Is(err, errRequestNotFound) {
		return info, err
	}
	if werr == nil && enc == nil {
		start() // 条目存在但没有帧（例如归档前即为空文件）：输出只含魔数的流
	}
	var cerr error
	if enc != nil {
		cerr = enc.Close()
	}
	if werr != nil {
		return info, werr
	}
	if err != nil {
		return info, err
	}
	if cerr != nil {
		return info, fmt.Errorf("audit: bundle encode: %w", cerr)
	}
	return info, nil
}
