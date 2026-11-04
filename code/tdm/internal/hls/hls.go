// Package hls 实现 m3u8 视频流下载：主/子播放列表解析、分片并发下载、
// AES-128 解密、断点续传（分片级）、合并与可选 ffmpeg remux。
package hls

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"tdm/internal/config"
	"tdm/internal/task"
)

const ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// Engine HLS 下载引擎
type Engine struct {
	cfg *config.Config
}

// New 创建引擎
func New(cfg *config.Config) *Engine { return &Engine{cfg: cfg} }

// ---------- 播放列表结构 ----------

type variant struct {
	bandwidth  int64
	resolution string
	uri        string
}

type keyInfo struct {
	method string
	uri    string // 密钥 URL
	iv     []byte // 显式 IV
}

type segment struct {
	uri      string
	duration float64
	key      *keyInfo
}

type playlist struct {
	variants []variant
	segments []segment
	live     bool
	mapURI   string // EXT-X-MAP 初始化段（fMP4）
}

// Run 阻塞执行 HLS 下载
func (e *Engine) Run(ctx context.Context, t *task.Task, upd task.Updater) error {
	client := e.client()

	body, err := fetchText(ctx, client, t.URL)
	if err != nil {
		return fmt.Errorf("获取 m3u8 失败: %w", err)
	}
	pl := parse(body)

	// 主播放列表：选最高码率（对标"最高 4K"）
	if len(pl.segments) == 0 && len(pl.variants) > 0 {
		best := pl.variants[0]
		for _, v := range pl.variants[1:] {
			if v.bandwidth > best.bandwidth {
				best = v
			}
		}
		body, err = fetchText(ctx, client, resolveURL(t.URL, best.uri))
		if err != nil {
			return fmt.Errorf("获取子播放列表失败: %w", err)
		}
		pl = parse(body)
	}
	if len(pl.segments) == 0 {
		return fmt.Errorf("未解析到任何分片")
	}
	if pl.live {
		return fmt.Errorf("直播流暂不支持")
	}

	// 目标路径与分片目录
	name := t.Filename
	if name == "" {
		name = guessName(t.URL)
	}
	if name == "" {
		name = fmt.Sprintf("hls_%d.ts", time.Now().Unix())
	}
	if err := os.MkdirAll(t.Dir, 0o755); err != nil {
		return err
	}
	stem := filepath.Join(t.Dir, name)
	tmpDir := stem + ".tdm.parts"
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	// 密钥缓存
	var keyMu sync.Mutex
	keyCache := map[string][]byte{}
	getKey := func(ku string) ([]byte, error) {
		keyMu.Lock()
		defer keyMu.Unlock()
		if k, ok := keyCache[ku]; ok {
			return k, nil
		}
		k, err := fetchBytesRetry(ctx, client, ku, 2)
		if err != nil {
			return nil, err
		}
		keyCache[ku] = k
		return k, nil
	}

	segs := pl.segments
	total := int64(len(segs))
	upd(func(x *task.Task) {
		if x.Title == "" {
			x.Title = strings.TrimSuffix(filepath.Base(stem), filepath.Ext(stem))
		}
		x.Total = -1
		x.Progress = 0
	})

	// 统计：已完成分片数 + 已下载字节
	var doneSegs atomic.Int64
	var gotBytes atomic.Int64
	for i := range segs {
		if fi, err := os.Stat(segPath(tmpDir, i)); err == nil && fi.Size() > 0 {
			doneSegs.Add(1)
			gotBytes.Add(fi.Size())
		}
	}

	if doneSegs.Load() < total {
		stop := trackSpeed(ctx, upd, &gotBytes)
		conc := e.cfg.HlsConcurrency
		if conc < 1 {
			conc = 1
		}
		idxCh := make(chan int)
		var wg sync.WaitGroup
		var fail atomic.Value
		for w := 0; w < conc; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range idxCh {
					if ctx.Err() != nil {
						return
					}
					if err := e.fetchSegment(ctx, client, segs[i], resolveURL(t.URL, segs[i].uri), segPath(tmpDir, i), i, getKey); err != nil {
						if ctx.Err() != nil {
							return
						}
						fail.Store(err)
						return
					}
					if fi, err := os.Stat(segPath(tmpDir, i)); err == nil {
						gotBytes.Add(fi.Size())
					}
					n := doneSegs.Add(1)
					upd(func(x *task.Task) {
						x.Done = n
						x.Progress = float64(n) / float64(total) * 100
					})
				}
			}()
		}
		for i := range segs {
			if _, err := os.Stat(segPath(tmpDir, i)); err != nil {
				idxCh <- i
			}
		}
		close(idxCh)
		wg.Wait()
		stop()
		if err, ok := fail.Load().(error); ok && err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	// 合并输出
	var finalPath string
	ffmpeg := e.ffmpegPath()
	wantMP4 := e.cfg.PreferMP4 && ffmpeg != ""
	if strings.HasSuffix(strings.ToLower(name), ".mp4") || (pl.mapURI != "" && !strings.HasSuffix(strings.ToLower(name), ".ts")) {
		finalPath = ensureUnique(stem)
		if pl.mapURI != "" {
			// fMP4：先落初始化段再拼接
			if err := e.fetchSegment(ctx, client, segment{uri: pl.mapURI}, resolveURL(t.URL, pl.mapURI), segPath(tmpDir, -1), -1, getKey); err != nil {
				return fmt.Errorf("初始化段失败: %w", err)
			}
		}
	} else if wantMP4 {
		finalPath = ensureUnique(strings.TrimSuffix(stem, filepath.Ext(stem)) + ".mp4")
	} else {
		finalPath = ensureUnique(ensureTS(stem))
	}

	if wantMP4 && strings.HasSuffix(finalPath, ".mp4") {
		if err := ffmpegRemux(ctx, tmpDir, int(total), finalPath, ffmpeg, pl.mapURI != ""); err == nil {
			upd(func(x *task.Task) { x.FilePath = finalPath; x.Progress = 100 })
			return nil
		}
		// remux 失败退回 TS 直拼
		finalPath = ensureUnique(strings.TrimSuffix(finalPath, filepath.Ext(finalPath)) + ".ts")
	}

	if err := concatSegs(tmpDir, int(total), finalPath, pl.mapURI != ""); err != nil {
		return err
	}
	upd(func(x *task.Task) { x.FilePath = finalPath; x.Progress = 100 })
	return nil
}

// fetchSegment 下载单个分片（含 AES-128 解密）
func (e *Engine) fetchSegment(ctx context.Context, client *http.Client, seg segment, absURL, savePath string, idx int, getKey func(string) ([]byte, error)) error {
	data, err := fetchBytesRetry(ctx, client, absURL, 3)
	if err != nil {
		return fmt.Errorf("分片 %d: %w", idx, err)
	}
	if seg.key != nil && strings.EqualFold(seg.key.method, "AES-128") {
		key, err := getKey(seg.key.uri)
		if err != nil {
			return fmt.Errorf("分片 %d 取密钥失败: %w", idx, err)
		}
		iv := seg.key.iv
		if iv == nil {
			// 默认 IV = 媒体序列号（BigEndian）；点播常见 seq 连续，以分片序号近似
			iv = make([]byte, 16)
			binary.BigEndian.PutUint64(iv[8:], uint64(idx))
		}
		data, err = aes128Decrypt(key, iv, data)
		if err != nil {
			return fmt.Errorf("分片 %d 解密失败: %w", idx, err)
		}
	}
	return os.WriteFile(savePath, data, 0o644)
}

// ---------- m3u8 解析 ----------

func parse(content string) playlist {
	pl := playlist{}
	lines := strings.Split(content, "\n")
	var curKey *keyInfo
	var pendingDur float64
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			pl.segments = append(pl.segments, segment{uri: line, duration: pendingDur, key: curKey})
			continue
		}
		tag, val := line, ""
		if idx := strings.Index(line, ":"); idx > 0 {
			tag, val = line[:idx], line[idx+1:]
		}
		switch tag {
		case "#EXT-X-STREAM-INF":
			v := variant{}
			for _, attr := range parseAttrs(val) {
				switch attr.k {
				case "BANDWIDTH":
					v.bandwidth, _ = strconv.ParseInt(attr.v, 10, 64)
				case "RESOLUTION":
					v.resolution = attr.v
				}
			}
			for j := i + 1; j < len(lines); j++ {
				nl := strings.TrimSpace(lines[j])
				if nl != "" && !strings.HasPrefix(nl, "#") {
					v.uri = nl
					i = j
					break
				}
			}
			if v.uri != "" {
				pl.variants = append(pl.variants, v)
			}
		case "#EXTINF":
			pendingDur, _ = strconv.ParseFloat(strings.TrimSuffix(val, ","), 64)
		case "#EXT-X-KEY":
			k := &keyInfo{}
			for _, attr := range parseAttrs(val) {
				switch attr.k {
				case "METHOD":
					k.method = attr.v
				case "URI":
					k.uri = attr.v
				case "IV":
					if s := strings.TrimPrefix(strings.ToLower(attr.v), "0x"); s != "" {
						if b, err := hex.DecodeString(s); err == nil {
							k.iv = b
						}
					}
				}
			}
			if strings.EqualFold(k.method, "NONE") {
				curKey = nil
			} else {
				curKey = k
			}
		case "#EXT-X-MAP":
			for _, attr := range parseAttrs(val) {
				if attr.k == "URI" {
					pl.mapURI = attr.v
				}
			}
		}
	}
	pl.live = !strings.Contains(content, "#EXT-X-ENDLIST") && len(pl.segments) > 0
	return pl
}

type attr struct{ k, v string }

func parseAttrs(s string) []attr {
	var out []attr
	i := 0
	for i < len(s) {
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			break
		}
		k := s[i : i+eq]
		i += eq + 1
		var v string
		if i < len(s) && s[i] == '"' {
			end := strings.IndexByte(s[i+1:], '"')
			if end < 0 {
				break
			}
			v = s[i+1 : i+1+end]
			i += end + 2
		} else {
			end := strings.IndexByte(s[i:], ',')
			if end < 0 {
				v, i = s[i:], len(s)
			} else {
				v, i = s[i:i+end], i+end+1
			}
		}
		out = append(out, attr{k: strings.TrimSpace(k), v: strings.TrimSpace(v)})
		for i < len(s) && s[i] == ',' {
			i++
		}
	}
	return out
}

// ---------- 工具 ----------

func (e *Engine) client() *http.Client {
	tr := &http.Transport{MaxIdleConnsPerHost: 32, IdleConnTimeout: 60 * time.Second}
	if e.cfg.Proxy != "" {
		if pu, err := url.Parse(e.cfg.Proxy); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	return &http.Client{Transport: tr}
}

func fetchText(ctx context.Context, client *http.Client, raw string) (string, error) {
	b, err := fetchBytesRetry(ctx, client, raw, 2)
	return string(b), err
}

func fetchBytes(ctx context.Context, client *http.Client, raw string) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	req.Header.Set("User-Agent", ua)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func fetchBytesRetry(ctx context.Context, client *http.Client, raw string, n int) ([]byte, error) {
	var err error
	for i := 0; i <= n; i++ {
		var b []byte
		b, err = fetchBytes(ctx, client, raw)
		if err == nil {
			return b, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		select {
		case <-time.After(time.Duration(i+1) * time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, err
}

func resolveURL(base, ref string) string {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

func segPath(dir string, i int) string {
	return filepath.Join(dir, fmt.Sprintf("seg_%06d.bin", i+100000)) // -1 → 099999 不冲突
}

func guessName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	base := filepath.Base(u.Path)
	base = strings.TrimSuffix(base, ".m3u8")
	if dec, err := url.PathUnescape(base); err == nil {
		base = dec
	}
	base = strings.Map(func(r rune) rune {
		switch r {
		case '\\', '/', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		return r
	}, base)
	if base == "" || base == "." {
		return ""
	}
	return base + ".ts"
}

func ensureTS(stem string) string {
	if strings.HasSuffix(strings.ToLower(stem), ".ts") {
		return stem
	}
	return stem + ".ts"
}

func ensureUnique(p string) string {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	stem := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		c := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if _, err := os.Stat(c); os.IsNotExist(err) {
			return c
		}
	}
}

func aes128Decrypt(key, iv, data []byte) ([]byte, error) {
	if len(key) != 16 {
		return nil, fmt.Errorf("密钥长度 %d 非 16 字节", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("密文长度 %d 非 16 的倍数", len(data))
	}
	mode := cipher.NewCBCDecrypter(block, iv)
	out := make([]byte, len(data))
	mode.CryptBlocks(out, data)
	if n := int(out[len(out)-1]); n > 0 && n <= 16 { // 去 PKCS7
		out = out[:len(out)-n]
	}
	return out, nil
}

func concatSegs(dir string, total int, outPath string, withInit bool) error {
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	if withInit { // fMP4 初始化段置首
		b, err := os.ReadFile(segPath(dir, -1))
		if err == nil {
			_, _ = w.Write(b)
		}
	}
	for i := 0; i < total; i++ {
		b, err := os.ReadFile(segPath(dir, i))
		if err != nil {
			return fmt.Errorf("分片 %d 缺失: %w", i, err)
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	return w.Flush()
}

func trackSpeed(ctx context.Context, upd task.Updater, got *atomic.Int64) func() {
	tk := time.NewTicker(500 * time.Millisecond)
	go func() {
		prev, prevAt := got.Load(), time.Now()
		var ema float64
		for {
			select {
			case <-ctx.Done():
				tk.Stop()
				return
			case <-tk.C:
				cur := got.Load()
				now := time.Now()
				dt := now.Sub(prevAt).Seconds()
				if dt > 0.2 {
					inst := float64(cur-prev) / dt
					if ema == 0 {
						ema = inst
					} else {
						ema = ema*0.6 + inst*0.4
					}
					prev, prevAt = cur, now
				}
				upd(func(x *task.Task) { x.Speed = ema })
			}
		}
	}()
	return func() { tk.Stop() }
}

// ffmpegPath 查找 ffmpeg（bin 目录优先，其次 PATH）
func (e *Engine) ffmpegPath() string {
	cand := filepath.Join(config.BinDir(), "ffmpeg.exe")
	if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
		return cand
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return ""
}

// ffmpegRemux 用 ffmpeg 按分片顺序合并并转封装为 mp4
func ffmpegRemux(ctx context.Context, dir string, total int, outPath, ffmpeg string, fmp4 bool) error {
	listPath := filepath.Join(dir, "concat.txt")
	var buf bytes.Buffer
	if fmp4 { // fMP4 初始化段置首
		if abs, err := filepath.Abs(segPath(dir, -1)); err == nil {
			buf.WriteString("file '" + strings.ReplaceAll(abs, "'", "'\\''") + "'\n")
		}
	}
	for i := 0; i < total; i++ {
		abs, _ := filepath.Abs(segPath(dir, i))
		buf.WriteString("file '" + strings.ReplaceAll(abs, "'", "'\\''") + "'\n")
	}
	if err := os.WriteFile(listPath, buf.Bytes(), 0o644); err != nil {
		return err
	}
	args := []string{"-y", "-f", "concat", "-safe", "0", "-i", listPath, "-c", "copy"}
	if !fmp4 {
		args = append(args, "-bsf:a", "aac_adtstoasc")
	}
	args = append(args, outPath)
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // 不创建控制台窗口
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg remux: %v: %s", err, string(out))
	}
	return os.Remove(listPath)
}
