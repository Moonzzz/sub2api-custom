package audit

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"io"
	"strings"
)

// wsDeflate 描述协商得到的 permessage-deflate 参数（RFC 7692）。
type wsDeflate struct {
	enabled                 bool
	serverNoContextTakeover bool
	clientNoContextTakeover bool
}

func parseWSExtensions(v string) wsDeflate {
	var d wsDeflate
	for _, ext := range strings.Split(v, ",") {
		params := strings.Split(ext, ";")
		if strings.TrimSpace(params[0]) != "permessage-deflate" {
			continue
		}
		d.enabled = true
		for _, p := range params[1:] {
			switch strings.TrimSpace(strings.SplitN(p, "=", 2)[0]) {
			case "server_no_context_takeover":
				d.serverNoContextTakeover = true
			case "client_no_context_takeover":
				d.clientNoContextTakeover = true
			}
		}
	}
	return d
}

// inflater 以“上一条消息末尾 32KB 明文”为预置字典解压每条消息，
// 等价于 context takeover 下的滑动窗口，展示阶段离线即可还原。
type inflater struct {
	takeover bool
	window   []byte
}

func (f *inflater) inflate(payload []byte) ([]byte, error) {
	in := append(append([]byte(nil), payload...), 0x00, 0x00, 0xff, 0xff)
	var dict []byte
	if f.takeover {
		dict = f.window
	}
	r := flate.NewReaderDict(bytes.NewReader(in), dict)
	out, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return out, err
	}
	if f.takeover {
		f.window = append(f.window, out...)
		if len(f.window) > 32<<10 {
			f.window = append([]byte(nil), f.window[len(f.window)-(32<<10):]...)
		}
	}
	return out, nil
}

// parseWSFrames 把劫持连接上的原始字节解析为消息（处理掩码、分片、压缩）。
// dir 为展示方向标签；masked 表示客户端→服务端方向。
func parseWSFrames(raw []byte, dir string, defl wsDeflate, clientToServer bool, limit int) ([]WSMessage, bool) {
	var msgs []WSMessage
	takeover := defl.enabled
	if clientToServer && defl.clientNoContextTakeover {
		takeover = false
	}
	if !clientToServer && defl.serverNoContextTakeover {
		takeover = false
	}
	inf := &inflater{takeover: takeover}

	var (
		msgOp         byte
		msgCompressed bool
		msgBuf        []byte
		inMessage     bool
	)
	pos := 0
	for pos < len(raw) {
		if len(msgs) >= limit {
			return msgs, true
		}
		if len(raw)-pos < 2 {
			return msgs, true
		}
		b0, b1 := raw[pos], raw[pos+1]
		fin := b0&0x80 != 0
		rsv1 := b0&0x40 != 0
		op := b0 & 0x0f
		masked := b1&0x80 != 0
		plen := uint64(b1 & 0x7f)
		hdr := 2
		switch plen {
		case 126:
			if len(raw)-pos < 4 {
				return msgs, true
			}
			plen = uint64(binary.BigEndian.Uint16(raw[pos+2:]))
			hdr = 4
		case 127:
			if len(raw)-pos < 10 {
				return msgs, true
			}
			plen = binary.BigEndian.Uint64(raw[pos+2:])
			hdr = 10
		}
		var key []byte
		if masked {
			if len(raw)-pos < hdr+4 {
				return msgs, true
			}
			key = raw[pos+hdr : pos+hdr+4]
			hdr += 4
		}
		if plen > uint64(len(raw)-pos-hdr) {
			// 帧不完整（连接中断或展示截断）。
			return msgs, true
		}
		payload := append([]byte(nil), raw[pos+hdr:pos+hdr+int(plen)]...) //nolint:gosec // 已校验边界
		if masked {
			for i := range payload {
				payload[i] ^= key[i%4]
			}
		}
		pos += hdr + int(plen) //nolint:gosec // 已校验边界

		if op >= 8 { // 控制帧：可插在分片之间
			m := WSMessage{Dir: dir, Op: opName(op), Size: len(payload)}
			if op == WSOpClose && len(payload) >= 2 {
				m.Text = strings.TrimSpace(strings.Join([]string{itoa(int(binary.BigEndian.Uint16(payload))), string(payload[2:])}, " "))
			} else {
				setPayload(&m.Text, &m.Base64, payload, 4096)
			}
			msgs = append(msgs, m)
			continue
		}
		if op != 0 {
			msgOp, msgCompressed, msgBuf, inMessage = op, rsv1 && defl.enabled, payload, true
		} else if inMessage {
			msgBuf = append(msgBuf, payload...)
		} else {
			continue
		}
		if !fin {
			continue
		}
		inMessage = false
		m := WSMessage{Dir: dir, Op: opName(msgOp), Compressed: msgCompressed}
		data := msgBuf
		if msgCompressed {
			out, err := inf.inflate(msgBuf)
			if err != nil {
				m.Error = "inflate: " + err.Error()
			}
			data = out
		}
		m.Size = len(data)
		setPayload(&m.Text, &m.Base64, data, 256<<10)
		msgs = append(msgs, m)
	}
	return msgs, false
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
