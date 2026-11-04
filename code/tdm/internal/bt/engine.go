package bt

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tdm/internal/config"
	"tdm/internal/task"
)

// ---------- 种子元信息 ----------

type fileEntry struct {
	Path   string `json:"path"`
	Length int64  `json:"length"`
	Offset int64  `json:"offset"`
}

type torrentInfo struct {
	Name        string      `json:"name"`
	PieceLen    int64       `json:"pieceLen"`
	Hashes      [][20]byte  `json:"-"`
	HashesHex   []string    `json:"hashes"`
	Files       []fileEntry `json:"files"`
	Total       int64       `json:"total"`
	InfoHash    [20]byte    `json:"-"`
	InfoHashHex string      `json:"infoHash"`
	RawInfo     []byte      `json:"-"`
}

// parseInfoDict 从 bencoded info 字典构建 torrentInfo（infoHash = sha1(raw)）
func parseInfoDict(raw []byte) (*torrentInfo, error) {
	v, err := Decode(raw)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("info 非字典")
	}
	ti := &torrentInfo{RawInfo: raw}
	h := sha1.Sum(raw)
	ti.InfoHash = h
	ti.InfoHashHex = hex.EncodeToString(h[:])

	if pl, ok := m["piece length"].(int64); ok && pl > 0 {
		ti.PieceLen = pl
	} else {
		return nil, fmt.Errorf("缺少 piece length")
	}
	pieces, ok := m["pieces"].(string)
	if !ok || len(pieces)%20 != 0 {
		return nil, fmt.Errorf("缺少 pieces")
	}
	n := len(pieces) / 20
	for i := 0; i < n; i++ {
		var ph [20]byte
		copy(ph[:], pieces[i*20:(i+1)*20])
		ti.Hashes = append(ti.Hashes, ph)
		ti.HashesHex = append(ti.HashesHex, hex.EncodeToString(ph[:]))
	}

	if files, ok := m["files"].([]any); ok { // 多文件
		name, _ := m["name"].(string)
		ti.Name = name
		var off int64
		for _, f := range files {
			fm, ok := f.(map[string]any)
			if !ok {
				continue
			}
			length, _ := fm["length"].(int64)
			var parts []string
			if ps, ok := fm["path"].([]any); ok {
				for _, p := range ps {
					if s, ok := p.(string); ok {
						parts = append(parts, s)
					}
				}
			}
			ti.Files = append(ti.Files, fileEntry{Path: filepath.Join(parts...), Length: length, Offset: off})
			off += length
		}
	} else { // 单文件
		name, _ := m["name"].(string)
		length, _ := m["length"].(int64)
		ti.Name = name
		ti.Files = []fileEntry{{Path: "", Length: length, Offset: 0}}
	}
	for _, f := range ti.Files {
		ti.Total += f.Length
	}
	if ti.Total <= 0 || len(ti.Hashes) == 0 {
		return nil, fmt.Errorf("种子信息不完整")
	}
	return ti, nil
}

// parseTorrentFile 解析完整 .torrent 文件
func parseTorrentFile(b []byte) (*torrentInfo, []string, error) {
	infoRaw, err := splitTopKey(b, "info")
	if err != nil {
		return nil, nil, fmt.Errorf("缺少 info 字段: %w", err)
	}
	ti, err := parseInfoDict(infoRaw)
	if err != nil {
		return nil, nil, err
	}
	v, err := Decode(b)
	if err != nil {
		return nil, nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("种子文件结构错误")
	}
	var trackers []string
	if als, ok := m["announce-list"].([]any); ok {
		for _, tier := range als {
			if tl, ok := tier.([]any); ok {
				for _, x := range tl {
					if s, ok := x.(string); ok {
						trackers = append(trackers, s)
					}
				}
			}
		}
	}
	if a, ok := m["announce"].(string); ok && a != "" {
		trackers = append(trackers, a)
	}
	return ti, trackers, nil
}

// ---------- 引擎 ----------

// Engine BT 下载引擎
type Engine struct {
	cfg *config.Config
}

// New 创建引擎
func New(cfg *config.Config) *Engine { return &Engine{cfg: cfg} }

// defaultTrackers 磁力通常不带 tr=，补充公共 tracker 提高 peer 发现率（对标迅雷/主流客户端的做法）
var defaultTrackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.demonii.com:1337/announce",
	"udp://tracker.openbittorrent.com:6969/announce",
	"udp://exodus.desync.com:6969/announce",
	"udp://tracker.torrent.eu.org:451/announce",
	"udp://tracker.moeking.me:6969/announce",
	"udp://open.stealth.si:80/announce",
	"udp://tracker.tiny-vps.com:6969/announce",
	"udp://tracker.cyberia.is:6969/announce",
	"https://tracker.opentrackr.org:443/announce",
}

// withDefaultTrackers 追加公共 tracker（去重）
func withDefaultTrackers(list []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(list)+len(defaultTrackers))
	for _, t := range list {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, t := range defaultTrackers {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// Run 阻塞执行 BT/磁力下载
func (e *Engine) Run(ctx context.Context, t *task.Task, upd task.Updater) error {
	// 单 peer 请求深度跟随"线程数"设置（BT 的并发体现为每个连接的在途块数）
	reqPerPeer := e.cfg.MaxThreads * 2
	if reqPerPeer < 10 {
		reqPerPeer = 10
	}
	if reqPerPeer > 64 {
		reqPerPeer = 64
	}
	s := &session{
		ctx: ctx, upd: upd, t: t, cfg: e.cfg,
		peerID: PeerID(), peers: map[*peer]struct{}{},
		metaPieces: map[int][]byte{}, files: map[string]*os.File{},
		metaCh:     make(chan []byte, 4),
		reqPerPeer: reqPerPeer,
	}
	defer s.closeAll()

	// 1. 输入解析
	switch {
	case strings.HasPrefix(t.URL, "magnet:"):
		ih, name, trackers, ok := ParseMagnet(t.URL)
		if !ok {
			return fmt.Errorf("磁力链接无效")
		}
		s.infoHash = ih
		s.trackers = withDefaultTrackers(trackers)
		s.pendingName = name
	default:
		data, err := fetchTorrent(ctx, t.URL)
		if err != nil {
			return fmt.Errorf("获取种子失败: %w", err)
		}
		ti, trackers, err := parseTorrentFile(data)
		if err != nil {
			return fmt.Errorf("解析种子失败: %w", err)
		}
		s.info = ti
		s.infoHash = ti.InfoHash
		s.trackers = withDefaultTrackers(trackers)
	}

	// 2. DHT
	if dht, err := NewDHT(); err == nil {
		s.dht = dht
		defer dht.Close()
		dht.PingAll()
	}

	// 2.1 入站监听：对端能主动连入后，冷门资源的 peer 数量会明显改善
	s.startListener()

	// 3. 输出布局与断点状态
	dir := t.Dir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if s.info != nil {
		if err := s.initOutput(); err != nil {
			return err
		}
		s.loadState()
		upd(func(x *task.Task) {
			x.Total = s.info.Total
			x.Title = s.info.Name
			x.FilePath = s.rootPath // 尽早落位，删除任务时可定位文件
			x.Done = s.doneBytes()
			x.Progress = float64(s.doneBytes()) / float64(s.info.Total) * 100
		})
	} else {
		upd(func(x *task.Task) {
			if s.pendingName != "" {
				x.Title = s.pendingName
			} else {
				x.Title = "magnet:" + hex.EncodeToString(s.infoHash[:8])
			}
			x.Total = -1
			x.Error = "" // 占位：元数据获取中
		})
	}

	// 4. peer 发现
	go s.discoveryLoop()

	// 5. 主循环
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()
	pexTick := time.NewTicker(45 * time.Second)
	defer pexTick.Stop()
	var ema float64
	prevBytes := s.doneBytes()
	prevAt := time.Now()

	for {
		select {
		case <-ctx.Done():
			s.saveState()
			return ctx.Err()
		case meta := <-s.metaCh:
			ti, err := parseInfoDict(meta)
			if err != nil || sha1.Sum(meta) != s.infoHash {
				s.metaMu.Lock()
				s.metaPieces = map[int][]byte{}
				s.metaMu.Unlock()
				continue
			}
			s.info = ti
			if err := s.initOutput(); err != nil {
				return err
			}
			s.loadState()
			s.parseAllBitfields()
			// 元数据就绪后必须立即给所有已连接 peer 排块：很多 peer 在拿到元数据之前
			// 就已经 unchoke 了，那次 refill 因为 info 为空被跳过，不补这一次速度会一直为 0。
			s.refillAll()
			prevBytes = s.doneBytes()
			prevAt = time.Now()
			upd(func(x *task.Task) {
				x.Total = ti.Total
				x.Title = ti.Name
				x.FilePath = s.rootPath // 尽早落位，删除任务时可定位文件
				x.Progress = float64(s.doneBytes()) / float64(ti.Total) * 100
			})
			s.fillPeers()
		case <-pexTick.C:
			// 定期向所有 peer 交换邻居列表（BEP-11），这是检索引擎里扩 peer 最快的途径
			s.pexAll()
		case <-tick.C:
			s.fillPeers()
			if s.info != nil {
				cur := s.doneBytes()
				now := time.Now()
				if dt := now.Sub(prevAt).Seconds(); dt > 0.5 {
					inst := float64(cur-prevBytes) / dt
					if ema == 0 {
						ema = inst
					} else {
						ema = ema*0.6 + inst*0.4
					}
					prevBytes, prevAt = cur, now
				}
				peers, seeds := s.peerStats()
				upd(func(x *task.Task) {
					x.Done = cur
					x.Total = s.info.Total
					x.Progress = float64(cur) / float64(s.info.Total) * 100
					x.Speed = ema
					x.Peers = peers
					x.Seeds = seeds
					if ema > 1024 && cur < s.info.Total {
						x.ETA = int64(float64(s.info.Total-cur) / ema)
					}
				})
				if s.haveCount() >= len(s.info.Hashes) {
					s.saveStateFinal()
					upd(func(x *task.Task) { x.FilePath = s.rootPath; x.Progress = 100; x.Speed = 0; x.ETA = 0 })
					return nil
				}
			}
		}
	}
}

// ---------- 会话 ----------

type session struct {
	ctx         context.Context
	upd         task.Updater
	t           *task.Task
	cfg         *config.Config
	peerID      [20]byte
	infoHash    [20]byte
	info        *torrentInfo
	trackers    []string
	pendingName string
	dht         *DHT
	dhtBusy     atomic.Bool // 防止 DHT 查找并发叠加
	listenPort  int
	ln          net.Listener

	mu        sync.Mutex
	peers     map[*peer]struct{}
	peerQ     []string
	tried     map[string]bool
	files     map[string]*os.File
	rootPath  string
	statePath string
	have      []bool
	pieces    []*pieceState
	avail     []int32

	stateMu  sync.Mutex  // 保护状态文件写入/删除的互斥
	finished atomic.Bool // 下载已完成，禁止再写状态文件

	metaCh     chan []byte
	metaMu     sync.Mutex
	metaPieces map[int][]byte
	metaSize   int

	reqPerPeer int // 单 peer 在途块上限
}

type pieceState struct {
	mu       sync.Mutex
	buf      []byte
	got      map[int]int64 // begin → len
	inflight map[int]int64 // begin → 请求时间戳(unix nano)
	filled   int64
}

func fetchTorrent(ctx context.Context, raw string) ([]byte, error) {
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	}
	return os.ReadFile(raw)
}

func sanitizeName(n string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\\', '/', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		return r
	}, n)
}

// initOutput 创建输出文件布局并初始化分片状态
func (s *session) initOutput() error {
	n := len(s.info.Hashes)
	s.have = make([]bool, n)
	s.pieces = make([]*pieceState, n)
	s.avail = make([]int32, n)
	s.rootPath = filepath.Join(s.t.Dir, sanitizeName(s.info.Name))
	s.statePath = s.rootPath + ".tdm.btstate"

	if len(s.info.Files) == 1 && s.info.Files[0].Path == "" {
		f, err := os.OpenFile(s.rootPath, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return err
		}
		s.files[s.rootPath] = f
		_ = f.Truncate(s.info.Files[0].Length)
	} else {
		if err := os.MkdirAll(s.rootPath, 0o755); err != nil {
			return err
		}
		for _, fe := range s.info.Files {
			p := filepath.Join(s.rootPath, fe.Path)
			if fe.Length == 0 {
				os.MkdirAll(filepath.Dir(p), 0o755)
				continue
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
			if err != nil {
				return err
			}
			s.files[p] = f
			_ = f.Truncate(fe.Length)
		}
	}
	return nil
}

func (s *session) pieceSize(i int) int64 {
	if i == len(s.info.Hashes)-1 {
		if r := s.info.Total - int64(i)*s.info.PieceLen; r > 0 {
			return r
		}
	}
	return s.info.PieceLen
}

func (s *session) doneBytes() int64 {
	if s.info == nil {
		return 0
	}
	var n int64
	for i, h := range s.have {
		if h {
			n += s.pieceSize(i)
		}
	}
	return n
}

func (s *session) haveCount() int {
	c := 0
	for _, h := range s.have {
		if h {
			c++
		}
	}
	return c
}

func (s *session) closeAll() {
	if s.ln != nil {
		s.ln.Close()
	}
	s.mu.Lock()
	for p := range s.peers {
		p.close()
	}
	s.mu.Unlock()
	for _, f := range s.files {
		f.Close()
	}
}

// ---------- peer 发现 ----------

func (s *session) discoveryLoop() {
	announce := func() {
		var left uint64
		if s.info != nil {
			left = uint64(s.info.Total - s.doneBytes())
		} else {
			// 元数据未知时不能用 0（会被 tracker 当成 seeder，返回的 peer 更少）
			left = 1 << 62
		}
		for _, tr := range s.trackers {
			go func(u string) {
				port := s.listenPort
				if port <= 0 {
					port = 6881
				}
				for _, a := range announcePeers(s.ctx, u, s.infoHash, s.peerID, port, left) {
					s.addPeerAddr(a)
				}
			}(tr)
		}
		if s.dht != nil && s.dhtBusy.CompareAndSwap(false, true) {
			go func() {
				defer s.dhtBusy.Store(false)
				// 发现一个 peer 就立刻建档：磁力阶段只要连上任意一个 peer 就能拿到元数据
				s.dht.GetPeers(s.infoHash, 40, 60*time.Second, s.addPeerAddr)
			}()
		}
	}
	announce()
	// 冷门资源最开始的几分钟最关键：先密集重试，再逐步退化为 2 分钟一次
	schedule := []time.Duration{8 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}
	idx := 0
	t := time.NewTimer(schedule[0])
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			announce()
			idx++
			next := 2 * time.Minute
			if idx < len(schedule) {
				next = schedule[idx]
			}
			t.Reset(next)
		}
	}
}

func (s *session) addPeerAddr(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tried == nil {
		s.tried = map[string]bool{}
	}
	if s.tried[addr] || len(s.peers) >= s.cfg.BtMaxPeers*2 {
		return
	}
	s.tried[addr] = true
	s.peerQ = append(s.peerQ, addr)
}

func (s *session) fillPeers() {
	s.mu.Lock()
	q := s.peerQ
	s.peerQ = nil
	np := len(s.peers)
	s.mu.Unlock()
	for _, addr := range q {
		if np >= s.cfg.BtMaxPeers {
			s.mu.Lock()
			s.peerQ = append(s.peerQ, addr)
			s.mu.Unlock()
			return
		}
		go s.connectPeer(addr)
		np++
	}
}

// ---------- 入站监听 ----------

// startListener 监听入站 peer 连接（6881-6891 优先，被占用则退回随机端口）。
// 迅雷等客户端能从公网连入是因为它们可被连接；不可连接的客户端拿到的 peer 会明显更少。
func (s *session) startListener() {
	for port := 6881; port <= 6891; port++ {
		ln, err := net.Listen("tcp4", fmt.Sprintf(":%d", port))
		if err == nil {
			s.listenPort = port
			s.ln = ln
			go s.acceptLoop(ln)
			return
		}
	}
	ln, err := net.Listen("tcp4", ":0")
	if err != nil {
		s.listenPort = 6881 // 无法监听也不影响出站下载
		return
	}
	if ta, ok := ln.Addr().(*net.TCPAddr); ok {
		s.listenPort = ta.Port
	}
	s.ln = ln
	go s.acceptLoop(ln)
}

func (s *session) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.ctx.Done():
			default:
				// 监听器被关闭（任务结束）
			}
			return
		}
		go s.acceptPeer(conn)
	}
}

// acceptPeer 处理对端主动连入：校验握手 → 回握手 → 注册并进入统一的读循环
func (s *session) acceptPeer(conn net.Conn) {
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
		if r := recover(); r != nil {
			conn.Close()
		}
	}()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	hs := make([]byte, 68)
	if _, err := io.ReadFull(conn, hs); err != nil {
		return
	}
	if hs[0] != 19 || string(hs[1:20]) != "BitTorrent protocol" {
		return
	}
	var ih [20]byte
	copy(ih[:], hs[28:48])
	if ih != s.infoHash {
		return
	}
	out := make([]byte, 0, 68)
	out = append(out, 19)
	out = append(out, "BitTorrent protocol"...)
	out = append(out, reserved[:]...)
	out = append(out, s.infoHash[:]...)
	out = append(out, s.peerID[:]...)
	if _, err := conn.Write(out); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})

	p := &peer{
		addr:        conn.RemoteAddr().String(),
		conn:        conn,
		br:          bufio.NewReaderSize(conn, 256*1024),
		extIDs:      map[string]byte{},
		peerChoking: true,
		amChoking:   true,
	}
	p.supportsExt = hs[25]&0x10 != 0
	if !s.registerPeer(p) {
		return
	}
	ok = true
	s.startPeer(p)
}

// registerPeer 在连接数上限内注册 peer
func (s *session) registerPeer(p *peer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil || len(s.peers) >= s.cfg.BtMaxPeers {
		return false
	}
	s.peers[p] = struct{}{}
	return true
}

// startPeer 发送扩展握手 + interested 并启动读循环
func (s *session) startPeer(p *peer) {
	port := s.listenPort
	if port <= 0 {
		port = 6881
	}
	metaSize := 0
	if s.info != nil {
		metaSize = len(s.info.RawInfo)
	}
	p.sendExtHandshakePort(metaSize, port)
	p.writeMsg(2, nil) // interested
	go s.readLoop(p)
}

// ---------- PEX (BEP-11) ----------

// onPex 处理对端发来的邻居列表（PEX 是客户端之间交换 peer 的主要途径）
func (s *session) onPex(payload []byte) {
	v, err := Decode(payload)
	if err != nil {
		return
	}
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	if adds, ok := m["added"].(string); ok {
		for i := 0; i+6 <= len(adds); i += 6 {
			if a := compact2addr(adds[i : i+6]); a != "" {
				s.addPeerAddr(a)
			}
		}
	}
}

// compactPeerList 把 ip:port 列表编码为 compact peer 串（4B IP + 2B port）
func compactPeerList(addrs []string) string {
	var b []byte
	for _, a := range addrs {
		host, pstr, err := net.SplitHostPort(a)
		if err != nil {
			continue
		}
		ip := net.ParseIP(host).To4()
		if ip == nil {
			continue
		}
		port, err := strconv.Atoi(pstr)
		if err != nil || port <= 0 || port > 65535 {
			continue
		}
		b = append(b, ip...)
		b = append(b, byte(port>>8), byte(port))
	}
	return string(b)
}

// pexSend 向指定 peer 发送当前已知邻居列表（最多 50 个）
func (s *session) pexSend(p *peer) {
	id := p.extIDs["ut_pex"]
	if id == 0 {
		return
	}
	s.mu.Lock()
	var addrs []string
	for q := range s.peers {
		if q != p {
			addrs = append(addrs, q.addr)
		}
	}
	s.mu.Unlock()
	if len(addrs) == 0 {
		return
	}
	if len(addrs) > 50 {
		addrs = addrs[:50]
	}
	cp := compactPeerList(addrs)
	if cp == "" {
		return
	}
	flags := make([]byte, len(cp)/6)
	_ = p.sendExtMsg(id, Encode(map[string]any{"added": cp, "added.f": string(flags)}))
}

func (s *session) pexAll() {
	s.mu.Lock()
	list := make([]*peer, 0, len(s.peers))
	for p := range s.peers {
		list = append(list, p)
	}
	s.mu.Unlock()
	for _, p := range list {
		s.pexSend(p)
	}
}

func (s *session) connectPeer(addr string) {
	p, err := dialPeer(addr, s.infoHash, s.peerID, 10*time.Second)
	if err != nil {
		// 解除 tried 标记，下次 announce 可重试（对端可能是临时不可达）
		s.mu.Lock()
		delete(s.tried, addr)
		s.mu.Unlock()
		return
	}
	if !s.registerPeer(p) {
		p.close()
		return
	}
	s.startPeer(p)
}

func (s *session) removePeer(p *peer) {
	s.mu.Lock()
	if _, ok := s.peers[p]; ok {
		delete(s.peers, p)
		if p.bitfield != nil && s.avail != nil {
			for i, h := range p.bitfield {
				if h && i < len(s.avail) {
					atomic.AddInt32(&s.avail[i], -1)
				}
			}
		}
	}
	s.mu.Unlock()
	p.close()
}

func (s *session) peerStats() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seeds := 0
	for p := range s.peers {
		if p.bitfield == nil || s.info == nil {
			continue
		}
		full := true
		for i := range s.info.Hashes {
			if i >= len(p.bitfield) || !p.bitfield[i] {
				full = false
				break
			}
		}
		if full {
			seeds++
		}
	}
	return len(s.peers), seeds
}

// ---------- peer 读循环 ----------

func (s *session) readLoop(p *peer) {
	defer s.removePeer(p)
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}
		p.conn.SetReadDeadline(time.Now().Add(3 * time.Minute))
		m, err := p.readMsg()
		if err != nil {
			return
		}
		switch m.id {
		case 255: // keepalive
		case 0:
			p.peerChoking = true
		case 1:
			p.peerChoking = false
			s.refill(p)
		case 2:
			p.peerInterested = true
			p.writeMsg(1, nil) // 慷慨 unchoke（上传服务）
		case 3:
			p.peerInterested = false
		case 4: // have
			if len(m.payload) == 4 {
				idx := binary.BigEndian.Uint32(m.payload)
				if s.info != nil && int(idx) < len(s.info.Hashes) {
					s.parseBitfieldFromRaw(p) // 确保 bitfield 存在
					if int(idx) < len(p.bitfield) && !p.bitfield[idx] {
						p.bitfield[idx] = true
						if int(idx) < len(s.avail) {
							atomic.AddInt32(&s.avail[idx], 1)
						}
					}
					s.refill(p)
				}
			}
		case 5: // bitfield（info 未知时暂存原始字节）
			if s.info != nil {
				s.applyBitfield(p, m.payload)
			} else {
				p.rawBitfield = append([]byte{}, m.payload...)
			}
			s.refill(p) // 拿到位图后立即排块（此前可能因 info 为空被跳过）
		case 6: // request（上传服务）
			if len(m.payload) == 12 {
				idx := binary.BigEndian.Uint32(m.payload[0:])
				begin := binary.BigEndian.Uint32(m.payload[4:])
				length := binary.BigEndian.Uint32(m.payload[8:])
				if length <= 128*1024 {
					if data, ok := s.readPieceBlock(int(idx), int64(begin), int64(length)); ok {
						p.sendPiece(idx, begin, data)
					}
				}
			}
		case 7:
			s.onPieceData(p, m.payload)
		case 20: // 扩展
			if len(m.payload) == 0 {
				continue
			}
			switch m.payload[0] {
			case 0: // 扩展握手
				p.parseExtHandshake(m.payload[1:])
				s.onExtHandshake(p)
			case 1: // ut_pex（我们在握手时声明的 id=1）
				s.onPex(m.payload[1:])
			case 2: // ut_metadata 数据（我们声明的 id=2）
				s.onMetadataPiece(m.payload[1:])
			}
		}
	}
}

// applyBitfield 解析 bitfield 载荷
func (s *session) applyBitfield(p *peer, payload []byte) {
	n := len(s.info.Hashes)
	if len(payload)*8 < n {
		return
	}
	p.bitfield = make([]bool, n)
	for i := 0; i < n; i++ {
		if payload[i/8]&(0x80>>(i%8)) != 0 {
			p.bitfield[i] = true
			if i < len(s.avail) {
				atomic.AddInt32(&s.avail[i], 1)
			}
		}
	}
}

// parseBitfieldFromRaw 元数据就绪后补解析此前暂存的 bitfield
func (s *session) parseBitfieldFromRaw(p *peer) {
	if p.bitfield != nil || p.rawBitfield == nil || s.info == nil {
		return
	}
	s.applyBitfield(p, p.rawBitfield)
}

func (s *session) parseAllBitfields() {
	s.mu.Lock()
	list := make([]*peer, 0, len(s.peers))
	for p := range s.peers {
		list = append(list, p)
	}
	s.mu.Unlock()
	for _, p := range list {
		s.parseBitfieldFromRaw(p)
	}
}

// onExtHandshake 对端扩展握手完成：交换 PEX 邻居列表，必要时请求磁力元数据
func (s *session) onExtHandshake(p *peer) {
	// 对端支持 ut_pex 就先交换一次邻居列表：PEX 是运行期最主要的 peer 扩充来源
	if p.extIDs["ut_pex"] != 0 {
		s.pexSend(p)
	}
	utID := p.extIDs["ut_metadata"]
	if s.info != nil || utID == 0 || p.metaSize <= 0 {
		return
	}
	s.metaMu.Lock()
	s.metaSize = p.metaSize
	np := (p.metaSize + blockSize - 1) / blockSize
	needed := make([]int, 0, np)
	for i := 0; i < np; i++ {
		if len(s.metaPieces[i]) == 0 {
			needed = append(needed, i)
		}
	}
	s.metaMu.Unlock()
	for _, piece := range needed {
		p.sendExtMsg(utID, Encode(map[string]any{"msg_type": 0, "piece": piece}))
	}
}

// onMetadataPiece 收到 ut_metadata 数据分片
func (s *session) onMetadataPiece(body []byte) {
	// 数据消息 = bencode 字典头 + 紧跟的元数据原始字节。
	// 必须用真正的 bencode 解析得到字典结束位置，绝不能用括号计数扫描——
	// 元数据二进制里充满 'd'/'e'/'l' 字节会导致切分错位、sha1 永不匹配。
	v, consumed, err := decode(body, 0)
	if err != nil || consumed <= 0 || consumed >= len(body) {
		return
	}
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	if mt, _ := m["msg_type"].(int64); mt != 1 {
		return
	}
	piece, _ := m["piece"].(int64)
	data := body[consumed:]

	s.metaMu.Lock()
	if piece >= 0 && len(data) > 0 {
		s.metaPieces[int(piece)] = append([]byte{}, data...)
	}
	total := s.metaSize
	np := 0
	if total > 0 {
		np = (total + blockSize - 1) / blockSize
	}
	complete := np > 0
	var buf []byte
	if complete {
		for i := 0; i < np; i++ {
			d, ok := s.metaPieces[i]
			if !ok || len(d) == 0 {
				complete = false
				break
			}
			buf = append(buf, d...)
		}
	}
	s.metaMu.Unlock()
	if complete {
		select {
		case s.metaCh <- buf:
		default:
		}
	}
}

// ---------- 分片下载 ----------

const blockTimeout = 45 * time.Second

// refillAll 给所有已连接 peer 重新排块（元数据就绪/位图更新后调用）
func (s *session) refillAll() {
	if s.info == nil {
		return
	}
	s.mu.Lock()
	list := make([]*peer, 0, len(s.peers))
	for p := range s.peers {
		list = append(list, p)
	}
	s.mu.Unlock()
	for _, p := range list {
		s.refill(p)
	}
}

func (s *session) refill(p *peer) {
	if s.info == nil {
		return
	}
	limit := int32(s.reqPerPeer)
	if limit <= 0 {
		limit = 10
	}
	for atomic.LoadInt32(&p.outstanding) < limit && !p.peerChoking {
		idx, begin, length, ok := s.assignBlock(p)
		if !ok {
			return
		}
		if err := p.requestBlock(uint32(idx), uint32(begin), uint32(length)); err != nil {
			return
		}
		atomic.AddInt32(&p.outstanding, 1)
	}
}

// assignBlock 选块：优先填满已开始的分片，再启动新分片。
// 不能只挑"未开始的分片"，否则每个分片永远只下第一块，永远无法完成整片校验。
func (s *session) assignBlock(p *peer) (idx int, begin int64, length int64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.bitfield == nil || s.info == nil {
		return 0, 0, 0, false
	}
	n := len(s.info.Hashes)
	now := time.Now().UnixNano()

	// 第一遍：已开始且未完成的分片，优先补齐
	for i := 0; i < n; i++ {
		ps := s.pieces[i]
		if ps == nil || s.have[i] || i >= len(p.bitfield) || !p.bitfield[i] {
			continue
		}
		if idx, begin, length, ok := s.takeBlockLocked(ps, i, now); ok {
			return idx, begin, length, true
		}
	}

	// 第二遍：启动新分片（rarest-first）
	candidates := []int{}
	bestAvail := int32(1 << 30)
	for i := 0; i < n; i++ {
		if s.have[i] || s.pieces[i] != nil || i >= len(p.bitfield) || !p.bitfield[i] {
			continue
		}
		a := atomic.LoadInt32(&s.avail[i])
		if a < bestAvail {
			bestAvail = a
			candidates = candidates[:0]
			candidates = append(candidates, i)
		} else if a == bestAvail {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return 0, 0, 0, false
	}
	pi := candidates[rand.Intn(len(candidates))]
	ps := &pieceState{got: map[int]int64{}, inflight: map[int]int64{}}
	s.pieces[pi] = ps
	if idx, begin, length, ok := s.takeBlockLocked(ps, pi, now); ok {
		return idx, begin, length, true
	}
	return 0, 0, 0, false
}

// takeBlockLocked 在指定分片里取一个未下载且未在途的块（调用方需持有 s.mu）
func (s *session) takeBlockLocked(ps *pieceState, pi int, now int64) (int, int64, int64, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	size := s.pieceSize(pi)
	for b := int64(0); b < size; b += blockSize {
		bi := int(b)
		if _, done := ps.got[bi]; done {
			continue
		}
		if ts, flying := ps.inflight[bi]; flying && now-ts < blockTimeout.Nanoseconds() {
			continue
		}
		l := int64(blockSize)
		if b+l > size {
			l = size - b
		}
		ps.inflight[bi] = now
		return pi, b, l, true
	}
	return 0, 0, 0, false
}

func (s *session) onPieceData(p *peer, payload []byte) {
	if len(payload) < 8 || s.info == nil {
		return
	}
	idx := int(binary.BigEndian.Uint32(payload[0:]))
	begin := int64(binary.BigEndian.Uint32(payload[4:]))
	data := payload[8:]
	if idx >= len(s.info.Hashes) {
		return
	}
	s.mu.Lock()
	if s.have[idx] {
		s.mu.Unlock()
		return
	}
	ps := s.pieces[idx]
	if ps == nil {
		ps = &pieceState{got: map[int]int64{}, inflight: map[int]int64{}}
		s.pieces[idx] = ps
	}
	s.mu.Unlock()

	ps.mu.Lock()
	size := s.pieceSize(idx)
	if ps.buf == nil {
		ps.buf = make([]byte, size)
	}
	wasInflight := false
	if begin >= 0 && begin+int64(len(data)) <= size {
		copy(ps.buf[begin:], data)
		bi := int(begin)
		if _, flying := ps.inflight[bi]; flying {
			wasInflight = true
			delete(ps.inflight, bi)
		}
		if _, done := ps.got[bi]; !done {
			ps.got[bi] = int64(len(data))
			ps.filled += int64(len(data))
		}
	}
	complete := ps.filled >= size
	var buf []byte
	if complete {
		buf = ps.buf[:size]
	}
	ps.mu.Unlock()

	if wasInflight {
		atomic.AddInt32(&p.outstanding, -1)
	}
	if complete {
		h := sha1.Sum(buf)
		if h == s.info.Hashes[idx] {
			if err := s.writePiece(idx, buf); err == nil {
				s.mu.Lock()
				s.have[idx] = true
				s.mu.Unlock()
				go s.saveState()
				s.broadcastHave(idx)
			} else {
				s.resetPiece(idx)
			}
		} else {
			s.resetPiece(idx)
		}
	}
	s.refill(p)
}

func (s *session) resetPiece(idx int) {
	s.mu.Lock()
	s.pieces[idx] = nil
	s.mu.Unlock()
}

func (s *session) broadcastHave(idx int) {
	s.mu.Lock()
	list := make([]*peer, 0, len(s.peers))
	for p := range s.peers {
		list = append(list, p)
	}
	s.mu.Unlock()
	for _, p := range list {
		p.sendHave(uint32(idx))
	}
}

// writePiece 将分片数据按全局偏移写入文件集
func (s *session) writePiece(idx int, data []byte) error {
	pOff := int64(idx) * s.info.PieceLen
	pEnd := pOff + int64(len(data))
	for _, fe := range s.info.Files {
		lo := fe.Offset
		if pOff > lo {
			lo = pOff
		}
		hi := fe.Offset + fe.Length
		if pEnd < hi {
			hi = pEnd
		}
		if lo >= hi {
			continue
		}
		f, ok := s.files[filepath.Join(s.rootPath, fe.Path)]
		if !ok {
			continue
		}
		if _, err := f.WriteAt(data[lo-pOff:hi-pOff], lo-fe.Offset); err != nil {
			return err
		}
	}
	return nil
}

// readPieceBlock 从磁盘读取块（上传服务）
func (s *session) readPieceBlock(idx int, begin, length int64) ([]byte, bool) {
	if s.info == nil || idx >= len(s.info.Hashes) {
		return nil, false
	}
	s.mu.Lock()
	if !s.have[idx] {
		s.mu.Unlock()
		return nil, false
	}
	s.mu.Unlock()

	off := int64(idx)*s.info.PieceLen + begin
	out := make([]byte, length)
	remaining := length
	for _, fe := range s.info.Files {
		lo := fe.Offset
		if off > lo {
			lo = off
		}
		hi := fe.Offset + fe.Length
		if off+length < hi {
			hi = off + length
		}
		if lo >= hi {
			continue
		}
		f, ok := s.files[filepath.Join(s.rootPath, fe.Path)]
		if !ok {
			continue
		}
		n := hi - lo
		if n > remaining {
			n = remaining
		}
		got, err := f.ReadAt(out[length-remaining:length-remaining+n], lo-fe.Offset)
		if err != nil && got == 0 {
			return nil, false
		}
		remaining -= int64(got)
	}
	return out, remaining == 0
}

// ---------- 状态持久化 ----------

type btStateJSON struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
	Have []int  `json:"have"`
}

func (s *session) saveState() {
	if s.info == nil || s.statePath == "" {
		return
	}
	if s.finished.Load() {
		return
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	// 双检：拿锁期间任务可能已完成
	if s.finished.Load() {
		return
	}
	s.mu.Lock()
	st := btStateJSON{Name: s.info.Name, Hash: s.info.InfoHashHex}
	for i, h := range s.have {
		if h {
			st.Have = append(st.Have, i)
		}
	}
	s.mu.Unlock()
	b, _ := json.Marshal(st)
	_ = os.WriteFile(s.statePath, b, 0o644)
}

func (s *session) saveStateFinal() {
	s.finished.Store(true)
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.statePath != "" {
		os.Remove(s.statePath)
	}
}

func (s *session) loadState() {
	if s.statePath == "" {
		return
	}
	b, err := os.ReadFile(s.statePath)
	if err != nil {
		return
	}
	var st btStateJSON
	if json.Unmarshal(b, &st) != nil || st.Hash != s.info.InfoHashHex {
		return
	}
	s.mu.Lock()
	for _, i := range st.Have {
		if i >= 0 && i < len(s.have) {
			s.have[i] = true
		}
	}
	s.mu.Unlock()
}
