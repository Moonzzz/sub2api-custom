package audit

import (
	"bufio"
	"errors"
	"os"
	"sync"

	"github.com/klauspost/compress/zstd"
)

var errFileClosed = errors.New("audit: archive file closed")

// codec 持有压缩器：一个并发安全的 EncodeAll 编码器（小文件一次性压缩）
// 和一个流式编码器池（大正文后台流式压缩，复用以控制内存）。
type codec struct {
	all     *zstd.Encoder
	streams sync.Pool
	opts    []zstd.EOption
}

func encoderLevel(level string) zstd.EncoderLevel {
	switch level {
	case "default":
		return zstd.SpeedDefault
	case "better":
		return zstd.SpeedBetterCompression
	case "best":
		return zstd.SpeedBestCompression
	}
	return zstd.SpeedFastest
}

// encoderOptions 返回压缩选项。windowMB 为匹配窗口（MB，2 的幂向上取整），
// 决定能回溯去重的最大距离，也决定解压内存（约 2 倍窗口）。
//
// lowMem：单请求文件（几 MB 以内、并发多）用低内存模式；会话归档（长流、单线程）
// 关闭低内存模式——实测 32MB 窗口下低内存模式压缩慢约 40 倍（15MB/s 对 550MB/s）。
func encoderOptions(level string, windowMB int, lowMem bool) []zstd.EOption {
	return []zstd.EOption{
		zstd.WithEncoderLevel(encoderLevel(level)),
		zstd.WithEncoderConcurrency(1),
		zstd.WithLowerEncoderMem(lowMem),
		zstd.WithWindowSize(windowBytes(windowMB)),
		zstd.WithEncoderCRC(true),
		zstd.WithZeroFrames(true),
	}
}

func windowBytes(mb int) int {
	if mb <= 0 {
		mb = 1
	}
	w := zstd.MinWindowSize
	for w < mb<<20 && w < maxWindowBytes {
		w <<= 1
	}
	return w
}

// maxWindowBytes 是允许的最大匹配窗口；读取端按此值限制解压内存。
const maxWindowBytes = 128 << 20

func newCodec(level string, windowMB int) (*codec, error) {
	opts := encoderOptions(level, windowMB, true)
	all, err := zstd.NewWriter(nil, opts...)
	if err != nil {
		return nil, err
	}
	return &codec{all: all, opts: opts}, nil
}

func (c *codec) getStream(w *bufio.Writer) (*zstd.Encoder, error) {
	if enc, ok := c.streams.Get().(*zstd.Encoder); ok {
		enc.Reset(w)
		return enc, nil
	}
	return zstd.NewWriter(w, c.opts...)
}

func (c *codec) putStream(enc *zstd.Encoder) {
	enc.Reset(nil)
	c.streams.Put(enc)
}

// archiveFile 是单个请求的 .zst 文件写入端（四个阶段共用一条压缩流）。
//
// 写入路径只做内存追加（拷贝），不在业务 goroutine 上压缩，也不做任何文件系统操作：
//   - 原始内容不超过 threshold 时，关闭阶段一次性 EncodeAll；
//   - 超过后切换为流式模式，由专属 goroutine 后台压缩落盘，
//     业务侧仍只是入队，SSE/大正文不受压缩速度影响。
type archiveFile struct {
	mu        sync.Mutex
	codec     *codec
	open      func() (*os.File, error) // 延迟创建文件（后台 goroutine 中调用）
	threshold int
	buf       []byte
	closed    bool
	raw       int64

	// 流式模式
	streaming bool
	cond      *sync.Cond
	queue     [][]byte
	finishing bool
	done      chan error
}

func newArchiveFile(open func() (*os.File, error), c *codec, threshold int) *archiveFile {
	af := &archiveFile{codec: c, open: open, threshold: threshold}
	af.buf = append(make([]byte, 0, 4096), requestMagic...)
	af.raw = int64(len(requestMagic))
	return af
}

// writeFrame 追加一个帧；payload 超过 maxFramePayload 时自动拆分。
func (a *archiveFile) writeFrame(typ, kind byte, seq uint16, payload []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return errFileClosed
	}
	for {
		n := len(payload)
		if n > maxFramePayload {
			n = maxFramePayload
		}
		a.appendLocked(typ, kind, seq, payload[:n])
		payload = payload[n:]
		if len(payload) == 0 {
			return nil
		}
	}
}

func (a *archiveFile) appendLocked(typ, kind byte, seq uint16, payload []byte) {
	a.raw += int64(frameHeaderSize + len(payload))
	if !a.streaming {
		a.buf = encodeFrameHeader(a.buf, typ, kind, seq, len(payload))
		a.buf = append(a.buf, payload...)
		if len(a.buf) > a.threshold {
			a.startStreamLocked()
		}
		return
	}
	chunk := make([]byte, 0, frameHeaderSize+len(payload))
	chunk = encodeFrameHeader(chunk, typ, kind, seq, len(payload))
	chunk = append(chunk, payload...)
	a.queue = append(a.queue, chunk)
	a.cond.Signal()
}

func (a *archiveFile) startStreamLocked() {
	a.streaming = true
	a.cond = sync.NewCond(&a.mu)
	a.queue = append(a.queue, a.buf)
	a.buf = nil
	a.done = make(chan error, 1)
	go a.streamLoop()
}

func (a *archiveFile) streamLoop() {
	f, err := a.open()
	var bw *bufio.Writer
	var enc *zstd.Encoder
	if err == nil {
		bw = bufio.NewWriterSize(f, 64<<10)
		enc, err = a.codec.getStream(bw)
	}
	for {
		a.mu.Lock()
		for len(a.queue) == 0 && !a.finishing {
			a.cond.Wait()
		}
		batch := a.queue
		a.queue = nil
		finishing := a.finishing
		a.mu.Unlock()

		for _, chunk := range batch {
			if err == nil {
				_, err = enc.Write(chunk)
			}
		}
		if finishing && len(batch) == 0 {
			break
		}
	}
	if enc != nil {
		if cerr := enc.Close(); err == nil {
			err = cerr
		}
		a.codec.putStream(enc)
	}
	if bw != nil {
		if ferr := bw.Flush(); err == nil {
			err = ferr
		}
	}
	if f != nil {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	a.done <- err
}

// close 完成压缩并关闭文件；可重复调用。
func (a *archiveFile) close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	if a.streaming {
		a.finishing = true
		a.cond.Signal()
		a.mu.Unlock()
		return <-a.done
	}
	buf := a.buf
	a.buf = nil
	a.mu.Unlock()

	f, err := a.open()
	if err != nil {
		return err
	}
	out := a.codec.all.EncodeAll(buf, make([]byte, 0, len(buf)/3+64))
	_, err = f.Write(out)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (a *archiveFile) rawSize() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.raw
}
