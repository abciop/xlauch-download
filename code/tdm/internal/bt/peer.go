package bt

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const blockSize = 16 * 1024

// peer 表示一条已握手的 peer 连接
type peer struct {
	addr        string
	conn        net.Conn
	br          *bufio.Reader
	wmu         sync.Mutex
	extIDs      map[string]byte // 扩展消息名 → 对端 id
	metaSize    int             // ut_metadata 元数据总长
	bitfield    []bool
	rawBitfield []byte // info 未知时暂存的原始 bitfield
	outstanding int32  // 在途块请求计数

	// 对端状态
	peerChoking    bool
	peerInterested bool
	amChoking      bool
	amInterested   bool
	supportsExt    bool
}

// reserved 扩展协议位（BEP-10 在第 5 字节 0x10）+ DHT（第 7 字节 0x01）
var reserved = [8]byte{0, 0, 0, 0, 0, 0x10, 0, 0x01}

// dialPeer 建立连接并完成 BitTorrent 握手
func dialPeer(addr string, infoHash, peerID [20]byte, timeout time.Duration) (*peer, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(60 * time.Second))
	p := &peer{
		addr:        addr,
		conn:        conn,
		br:          bufio.NewReaderSize(conn, 256*1024),
		extIDs:      map[string]byte{},
		peerChoking: true,
		amChoking:   true,
	}
	// 发送握手
	hs := make([]byte, 0, 68)
	hs = append(hs, 19)
	hs = append(hs, "BitTorrent protocol"...)
	hs = append(hs, reserved[:]...)
	hs = append(hs, infoHash[:]...)
	hs = append(hs, peerID[:]...)
	if _, err := conn.Write(hs); err != nil {
		conn.Close()
		return nil, err
	}
	// 读取对端握手
	rhs := make([]byte, 68)
	if _, err := io.ReadFull(p.br, rhs); err != nil {
		conn.Close()
		return nil, err
	}
	if rhs[0] != 19 || string(rhs[1:20]) != "BitTorrent protocol" {
		conn.Close()
		return nil, fmt.Errorf("非法握手")
	}
	p.supportsExt = rhs[25]&0x10 != 0
	// 握手完成后清除整体 deadline：否则 60s 后写超时到期，
	// 后续所有块请求都会立即失败（长任务必然断流）。读超时由 readLoop 逐次设置。
	conn.SetDeadline(time.Time{})
	return p, nil
}

// msg 单条 peer 消息
type msg struct {
	len     uint32
	id      byte
	payload []byte
}

// readMsg 读取一条消息（0 长度 = keepalive，返回 id=255）
func (p *peer) readMsg() (*msg, error) {
	var lb [4]byte
	if _, err := io.ReadFull(p.br, lb[:]); err != nil {
		return nil, err
	}
	l := binary.BigEndian.Uint32(lb[:])
	if l == 0 {
		return &msg{id: 255}, nil // keepalive
	}
	if l > 1+4*1024*1024 { // 上限保护
		return nil, fmt.Errorf("消息过大 %d", l)
	}
	buf := make([]byte, l)
	if _, err := io.ReadFull(p.br, buf); err != nil {
		return nil, err
	}
	return &msg{id: buf[0], payload: buf[1:], len: l}, nil
}

// writeMsg 发送一条消息
func (p *peer) writeMsg(id byte, payload []byte) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	l := uint32(len(payload) + 1)
	hdr := make([]byte, 5)
	binary.BigEndian.PutUint32(hdr, l)
	hdr[4] = id
	if _, err := p.conn.Write(hdr); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := p.conn.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// sendExtHandshake 发送扩展握手（BEP-10），声明支持 ut_metadata / ut_pex
func (p *peer) sendExtHandshake(metaSize int) error {
	return p.sendExtHandshakePort(metaSize, 6881)
}

// sendExtHandshakePort 同 sendExtHandshake，可指定对外声明的监听端口
func (p *peer) sendExtHandshakePort(metaSize, port int) error {
	m := map[string]any{
		"m": map[string]any{"ut_metadata": 2, "ut_pex": 1},
		"v": "Xlauch 1.0",
		"p": port,
	}
	if metaSize > 0 {
		m["metadata_size"] = metaSize
	}
	payload := append([]byte{0}, Encode(m)...) // ext id 0 = 扩展握手
	return p.writeMsg(20, payload)
}

// sendExtMsg 以指定扩展 id 发送 bencode 载荷
func (p *peer) sendExtMsg(extID byte, payload []byte) error {
	return p.writeMsg(20, append([]byte{extID}, payload...))
}

// parseExtHandshake 解析对端扩展握手
func (p *peer) parseExtHandshake(payload []byte) {
	v, err := Decode(payload)
	if err != nil {
		return
	}
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	if ms, ok := m["metadata_size"].(int64); ok {
		p.metaSize = int(ms)
	}
	if em, ok := m["m"].(map[string]any); ok {
		for name, id := range em {
			if b, ok := id.(int64); ok {
				p.extIDs[name] = byte(b)
			}
		}
	}
}

// requestBlock 发送块请求
func (p *peer) requestBlock(index, begin, length uint32) error {
	buf := make([]byte, 12)
	binary.BigEndian.PutUint32(buf[0:], index)
	binary.BigEndian.PutUint32(buf[4:], begin)
	binary.BigEndian.PutUint32(buf[8:], length)
	return p.writeMsg(6, buf)
}

// cancelBlock 取消块请求
func (p *peer) cancelBlock(index, begin, length uint32) error {
	buf := make([]byte, 12)
	binary.BigEndian.PutUint32(buf[0:], index)
	binary.BigEndian.PutUint32(buf[4:], begin)
	binary.BigEndian.PutUint32(buf[8:], length)
	return p.writeMsg(8, buf)
}

// sendHave 广播已获得分片
func (p *peer) sendHave(index uint32) error {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, index)
	return p.writeMsg(4, buf)
}

// sendPiece 响应上传请求
func (p *peer) sendPiece(index, begin uint32, data []byte) error {
	buf := make([]byte, 8+len(data))
	binary.BigEndian.PutUint32(buf[0:], index)
	binary.BigEndian.PutUint32(buf[4:], begin)
	copy(buf[8:], data)
	return p.writeMsg(7, buf)
}

func (p *peer) close() { p.conn.Close() }
