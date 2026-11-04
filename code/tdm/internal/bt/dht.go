package bt

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DHT 实现最小 KRPC 客户端（BEP-5）：迭代 get_peers 查找 peer。
// 只读节点（BEP-32）：不响应入站查询，仅主动查询。
type DHT struct {
	conn *net.UDPConn
	id   [20]byte

	txSeq uint32
	mu    sync.Mutex
	txs   map[string]chan map[string]any
	table map[string]string // addr → nodeID(hex)，简单路由表
	done  chan struct{}
}

// NewDHT 创建并启动 DHT 客户端
func NewDHT() (*DHT, error) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, err
	}
	d := &DHT{
		conn:  c,
		txs:   make(map[string]chan map[string]any),
		table: make(map[string]string),
		done:  make(chan struct{}),
	}
	rand.Read(d.id[:])
	d.id[0] |= 0x08 // read-only 标记
	go d.readLoop()
	return d, nil
}

// Close 关闭 DHT
func (d *DHT) Close() {
	close(d.done)
	d.conn.Close()
}

func (d *DHT) readLoop() {
	buf := make([]byte, 65536)
	for {
		n, addr, err := d.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-d.done:
				return
			default:
			}
			continue
		}
		v, err := Decode(buf[:n])
		if err != nil {
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		switch y, _ := m["y"].(string); y {
		case "r":
			r, _ := m["r"].(map[string]any)
			if r == nil {
				continue
			}
			// 所有响应节点都计入路由表（供后续查找作候选）
			if idb, ok := r["id"].(string); ok {
				d.mu.Lock()
				d.table[addr.String()] = hex.EncodeToString([]byte(idb))
				d.mu.Unlock()
			}
			// 事务匹配分发
			if tid, ok := m["t"].(string); ok {
				d.mu.Lock()
				ch, ok := d.txs[tid]
				if ok {
					delete(d.txs, tid)
				}
				d.mu.Unlock()
				if ok {
					select {
					case ch <- r:
					default:
					}
					continue
				}
			}
		case "q":
			// 响应入站查询：只应答、不回传节点。DHT 网络里长期不回复的节点
			// 会被其它节点当成死节点踢出路由表，后续 get_peers 拿到的节点越来越少。
			q, _ := m["q"].(string)
			tid, _ := m["t"].(string)
			if tid == "" {
				continue
			}
			switch q {
			case "ping":
				d.reply(addr, tid, map[string]any{"id": string(d.id[:])})
			case "get_peers":
				d.reply(addr, tid, map[string]any{"id": string(d.id[:]), "nodes": "", "token": "xdm"})
			case "announce_peer":
				d.reply(addr, tid, map[string]any{"id": string(d.id[:]), "token": "xdm"})
			}
		}
	}
}

// reply 应答入站 KRPC 查询（BEP-5 要求节点对 ping/get_peers 作出响应）
func (d *DHT) reply(addr *net.UDPAddr, tid string, r map[string]any) {
	payload := Encode(map[string]any{"t": tid, "y": "r", "r": r})
	_, _ = d.conn.WriteToUDP(payload, addr)
}

var bootstraps = []string{
	"dht.transmissionbt.com:6881", // 实测最稳定
	"router.bittorrent.com:6881",
	"router.utorrent.com:6881",
	"dht.libtorrent.org:25401",
	"dht.aelitis.com:6881", // Vuze
}

// Query 发送 KRPC 查询并等待响应（导出以便诊断/复用）
func (d *DHT) Query(addr string, method string, a map[string]any, timeout time.Duration) (map[string]any, error) {
	var tid [2]byte
	seq := atomic.AddUint32(&d.txSeq, 1)
	tid[0] = byte(seq >> 8)
	tid[1] = byte(seq)
	msg := map[string]any{
		"t": string(tid[:]),
		"y": "q",
		"q": method,
		"a": a,
	}
	payload := Encode(msg)
	ch := make(chan map[string]any, 1)
	key := string(tid[:])
	d.mu.Lock()
	d.txs[key] = ch
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.txs, key)
		d.mu.Unlock()
	}()

	raddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := d.conn.WriteToUDP(payload, raddr); err != nil {
			return nil, err
		}
		select {
		case r := <-ch:
			return r, nil
		case <-time.After(timeout):
		case <-d.done:
			return nil, fmt.Errorf("dht closed")
		}
	}
	return nil, fmt.Errorf("dht: %s %s 超时", method, addr)
}

// GetPeers 迭代查找拥有 infohash 的 peers，返回 ip:port 列表。
// 采用「候选队列 + 并发工作协程」的流水线方式：某个节点一返回更近的节点就立刻查询，
// 不必等整批查询结束，因此首个 peer 通常几秒内就能拿到（老实现要等 45 秒以上）。
// onPeer 非 nil 时每发现一个新 peer 立即回调；找到 minPeers 个即提前结束。
func (d *DHT) GetPeers(infoHash [20]byte, minPeers int, deadline time.Duration, onPeer func(string)) []string {
	ih := string(infoHash[:])
	if minPeers <= 0 {
		minPeers = 20
	}
	if deadline <= 0 {
		deadline = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	work := make(chan string, 4096)
	var candSeen sync.Map // addr → true
	var pending int64     // 队列中 + 正在查询的候选数

	enqueue := func(addr string) {
		if addr == "" {
			return
		}
		if _, loaded := candSeen.LoadOrStore(addr, true); loaded {
			return
		}
		atomic.AddInt64(&pending, 1)
		select {
		case work <- addr:
		default:
			atomic.AddInt64(&pending, -1)
		}
	}

	d.mu.Lock()
	for a := range d.table {
		enqueue(a)
	}
	d.mu.Unlock()
	for _, b := range bootstraps {
		enqueue(b)
	}

	peers := map[string]bool{}
	var mu sync.Mutex
	var once sync.Once
	var wg sync.WaitGroup

	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if atomic.LoadInt64(&pending) == 0 {
					return // 候选枯竭
				}
				var addr string
				select {
				case addr = <-work:
				case <-ctx.Done():
					return
				case <-time.After(200 * time.Millisecond):
					continue // 队列暂时空，回到循环重新检查 pending
				}
				r, err := d.Query(addr, "get_peers", map[string]any{"id": string(d.id[:]), "info_hash": ih}, 2*time.Second)

				var fresh []string
				if err == nil {
					mu.Lock()
					if vals, ok := r["values"].([]any); ok {
						for _, v := range vals {
							if s, ok := v.(string); ok && len(s) == 6 {
								a := compact2addr(s)
								if a != "" && !peers[a] {
									peers[a] = true
									fresh = append(fresh, a)
								}
							}
						}
					}
					mu.Unlock()
					if ns, ok := r["nodes"].(string); ok {
						for i := 0; i+26 <= len(ns); i += 26 {
							enqueue(compact2addr(ns[i+20 : i+26]))
						}
					}
				}
				atomic.AddInt64(&pending, -1)

				if onPeer != nil {
					for _, a := range fresh {
						onPeer(a)
					}
				}
				if len(fresh) > 0 {
					mu.Lock()
					n := len(peers)
					mu.Unlock()
					if n >= minPeers {
						once.Do(cancel) // 够用了，提前收工
					}
				}
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	out := make([]string, 0, len(peers))
	for p := range peers {
		out = append(out, p)
	}
	return out
}

// PingAll 预热路由表
func (d *DHT) PingAll() {
	for _, b := range bootstraps {
		go d.Query(b, "ping", map[string]any{"id": string(d.id[:])}, 2*time.Second)
	}
}

func compact2addr(c string) string {
	if len(c) < 6 {
		return ""
	}
	ip := net.IP([]byte(c[:4]))
	port := binary.BigEndian.Uint16([]byte(c[4:6]))
	if port == 0 {
		return ""
	}
	return net.JoinHostPort(ip.String(), fmt.Sprint(port))
}

// ParseMagnet 解析磁力链接
func ParseMagnet(uri string) (infoHash [20]byte, name string, trackers []string, ok bool) {
	if !strings.HasPrefix(uri, "magnet:?") {
		return infoHash, "", nil, false
	}
	for _, kv := range strings.Split(uri[8:], "&") {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		k, v := kv[:eq], kv[eq+1:]
		switch k {
		case "xt":
			if strings.HasPrefix(v, "urn:btih:") {
				h := v[9:]
				if len(h) == 40 {
					if b, err := hex.DecodeString(h); err == nil {
						copy(infoHash[:], b)
						ok = true
					}
				} else if len(h) == 32 { // base32
					if b, err := base32Decode(h); err == nil && len(b) == 20 {
						copy(infoHash[:], b)
						ok = true
					}
				}
			}
		case "dn":
			// dn 通常是 percent-encoded，解码后才是可读名称
			if dec, err := url.QueryUnescape(v); err == nil {
				name = dec
			} else {
				name = strings.ReplaceAll(v, "+", " ")
			}
		case "tr":
			// 网页里的 magnet 链接 tracker 参数通常是 percent-encoded
			// （udp%3A%2F%2Ftracker...），不解码会得到非法 URL、私有 tracker 全部失效
			if dec, err := url.QueryUnescape(v); err == nil {
				trackers = append(trackers, dec)
			} else {
				trackers = append(trackers, v)
			}
		}
	}
	return
}

// PeerID 生成 20 字节 peer id
func PeerID() [20]byte {
	var id [20]byte
	h := sha1.Sum([]byte(fmt.Sprint(time.Now().UnixNano())))
	copy(id[:], "-TD0100-")
	copy(id[8:], h[:12])
	return id
}

// base32 解码（磁力 btih 可能是 base32）
func base32Decode(s string) ([]byte, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	s = strings.ToUpper(strings.NewReplacer("=", "").Replace(s))
	var bits uint32
	var nbits uint
	out := []byte{}
	for _, c := range s {
		idx := strings.IndexByte(alphabet, byte(c))
		if idx < 0 {
			return nil, fmt.Errorf("base32: 非法字符 %q", c)
		}
		bits = bits<<5 | uint32(idx)
		nbits += 5
		if nbits >= 8 {
			nbits -= 8
			out = append(out, byte(bits>>nbits))
		}
	}
	return out, nil
}
