// Package httpdl 多线程分块 HTTP 下载引擎：Range 分块、断点续传、重试、限速统计。
package httpdl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tdm/internal/config"
	"tdm/internal/task"
)

const ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// Engine HTTP 下载引擎
type Engine struct {
	cfg *config.Config
}

// New 创建引擎
func New(cfg *config.Config) *Engine { return &Engine{cfg: cfg} }

// Run 阻塞执行下载任务
func (e *Engine) Run(ctx context.Context, t *task.Task, upd task.Updater) error {
	client := e.client()

	// 探测：大小 / Range 支持 / 文件名
	info, err := probe(ctx, client, t.URL)
	if err != nil {
		return err
	}

	name := t.Filename
	if name == "" {
		name = info.filename
	}
	if name == "" {
		name = "download"
	}
	if err := os.MkdirAll(t.Dir, 0o755); err != nil {
		return err
	}
	// 断点续传：任务已有落位路径时复用，避免恢复时被 uniquePath 改名，
	// 导致旧的部分文件成为无人认领的孤儿残留
	target := filepath.Join(t.Dir, name)
	if t.FilePath != "" {
		if _, err := os.Stat(t.FilePath); err == nil {
			target = t.FilePath // 原部分文件仍在，直接续传
		} else {
			target = uniquePath(t.FilePath) // 原文件已被移走/删除，防重名
		}
	} else {
		target = uniquePath(target) // 新任务，防重名
	}

	upd(func(x *task.Task) {
		x.Title = strings.TrimSuffix(filepath.Base(target), filepath.Ext(target))
		x.Total = info.size
		x.FilePath = target // 尽早落位，删除任务时可定位文件
		if info.size > 0 {
			x.Progress = 0
		}
	})

	// 不支持 Range 或大小未知 → 单流下载
	if !info.ranges || info.size <= 0 {
		return e.stream(ctx, client, t, upd, target, info)
	}

	return e.chunked(ctx, client, t, upd, target, info)
}

// ---------- 探测 ----------

type probeInfo struct {
	size     int64
	ranges   bool
	filename string
}

func (e *Engine) client() *http.Client {
	transport := &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	if e.cfg.Proxy != "" {
		if pu, err := url.Parse(e.cfg.Proxy); err == nil {
			transport.Proxy = http.ProxyURL(pu) // 支持 http/https/socks5
		}
	}
	return &http.Client{Transport: transport}
}

// setExtraHeaders 抖音 CDN 会校验来源页，必须补 Referer 才允许直链下载。
func setExtraHeaders(req *http.Request, raw string) {
	req.Header.Set("User-Agent", ua)
	u := strings.ToLower(raw)
	if strings.Contains(u, "douyin.com") || strings.Contains(u, "douyinvod.com") ||
		strings.Contains(u, "iesdouyin.com") || strings.Contains(u, "bytecdn.cn") ||
		strings.Contains(u, "zjcdn.com") {
		req.Header.Set("Referer", "https://www.douyin.com/")
	}
}

func probe(ctx context.Context, client *http.Client, raw string) (*probeInfo, error) {
	// 优先 ranged GET（比 HEAD 兼容性好）
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	setExtraHeaders(req, raw)
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	if err == nil && (resp.StatusCode == http.StatusPartialContent || resp.StatusCode == http.StatusOK) {
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		info := &probeInfo{filename: fileNameFrom(resp, raw)}
		if resp.StatusCode == http.StatusPartialContent {
			info.ranges = true
			if cr := resp.Header.Get("Content-Range"); strings.HasPrefix(cr, "bytes ") {
				if idx := strings.LastIndex(cr, "/"); idx >= 0 {
					fmt.Sscanf(cr[idx+1:], "%d", &info.size)
				}
			}
		} else if cl := resp.Header.Get("Content-Length"); cl != "" {
			fmt.Sscanf(cl, "%d", &info.size)
		}
		if info.size <= 0 {
			// 回退 HEAD
			hreq, _ := http.NewRequestWithContext(ctx, http.MethodHead, raw, nil)
			setExtraHeaders(hreq, raw)
			if hresp, err := client.Do(hreq); err == nil {
				defer hresp.Body.Close()
				if cl := hresp.Header.Get("Content-Length"); cl != "" {
					fmt.Sscanf(cl, "%d", &info.size)
				}
				if hresp.Header.Get("Accept-Ranges") == "bytes" {
					info.ranges = true
				}
				if info.filename == "" || info.filename == "download" {
					info.filename = fileNameFrom(hresp, raw)
				}
			}
		}
		return info, nil
	}
	// 最后尝试 HEAD
	hreq, _ := http.NewRequestWithContext(ctx, http.MethodHead, raw, nil)
	setExtraHeaders(hreq, raw)
	hresp, err := client.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("无法连接: %w", err)
	}
	defer hresp.Body.Close()
	info := &probeInfo{filename: fileNameFrom(hresp, raw)}
	if cl := hresp.Header.Get("Content-Length"); cl != "" {
		fmt.Sscanf(cl, "%d", &info.size)
	}
	if hresp.Header.Get("Accept-Ranges") == "bytes" && info.size > 0 {
		info.ranges = true
	}
	return info, nil
}

func fileNameFrom(resp *http.Response, rawURL string) string {
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if fn := params["filename"]; fn != "" {
				return sanitize(fn)
			}
			if fn, ok := params["filename*"]; ok {
				if i := strings.Index(fn, "''"); i >= 0 {
					if dec, err := url.PathUnescape(fn[i+2:]); err == nil {
						return sanitize(dec)
					}
				}
			}
		}
	}
	if u, err := url.Parse(rawURL); err == nil {
		base := filepath.Base(u.Path)
		if base != "" && base != "/" && base != "." {
			if dec, err := url.PathUnescape(base); err == nil {
				return sanitize(dec)
			}
			return sanitize(base)
		}
	}
	return ""
}

func sanitize(name string) string {
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return name
}

func uniquePath(p string) string {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	stem := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
}

// ---------- 单流下载 ----------

func (e *Engine) stream(ctx context.Context, client *http.Client, t *task.Task, upd task.Updater, target string, info *probeInfo) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, nil)
	setExtraHeaders(req, t.URL)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(target)
	if err != nil {
		return err
	}
	defer f.Close()

	if info.size > 0 {
		_ = f.Truncate(info.size)
	}

	var done atomic.Int64
	stop := trackProgress(ctx, t, upd, &done, info.size)
	defer stop()
	n, copyErr := io.Copy(f, resp.Body)
	done.Add(n)
	if copyErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return copyErr
	}
	upd(func(x *task.Task) { x.FilePath = target })
	return nil
}

// ---------- 分块多线程下载 ----------

type chunkState struct {
	URL        string   `json:"url"`
	Size       int64    `json:"size"`
	ChunkSize  int64    `json:"chunkSize"`
	Done       []bool   `json:"done"`
	FinalName  string   `json:"finalName"`
	ETag       string   `json:"etag"`
}

func (e *Engine) chunked(ctx context.Context, client *http.Client, t *task.Task, upd task.Updater, target string, info *probeInfo) error {
	statePath := target + ".tdm.state"

	threads := e.cfg.MaxThreads
	if threads < 1 {
		threads = 1
	}
	chunkSize := int64(1) << 20 // 1 MiB
	for {
		nChunks := (info.size + chunkSize - 1) / chunkSize
		if nChunks <= int64(threads*8) || chunkSize >= 64<<20 {
			break
		}
		chunkSize <<= 1
	}
	nChunks := (info.size + chunkSize - 1) / chunkSize

	var st chunkState
	if b, err := os.ReadFile(statePath); err == nil && json.Unmarshal(b, &st) == nil &&
		st.Size == info.size && st.ChunkSize == chunkSize && st.URL == t.URL && int64(len(st.Done)) == nChunks {
		// 断点续传：状态有效
	} else {
		st = chunkState{URL: t.URL, Size: info.size, ChunkSize: chunkSize, Done: make([]bool, nChunks), FinalName: target}
	}

	// 打开目标文件并预分配
	f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(info.size); err != nil {
		return err
	}

	// 进度基准：已完成块的字节数
	var base int64
	for i, ok := range st.Done {
		if ok {
			lo := int64(i) * chunkSize
			hi := min64(lo+chunkSize-1, info.size-1)
			base += hi - lo + 1
		}
	}
	var done atomic.Int64
	done.Store(base)
	upd(func(x *task.Task) {
		x.Done = base
		if info.size > 0 {
			x.Progress = float64(base) / float64(info.size) * 100
		}
	})

	stop := trackProgress(ctx, t, upd, &done, info.size)
	defer stop()

	// 工作队列
	var mu sync.Mutex // 保护 st.Done 与写入
	jobs := make(chan int64, nChunks)
	for i := int64(0); i < nChunks; i++ {
		if !st.Done[i] {
			jobs <- i
		}
	}
	close(jobs)

	var wg sync.WaitGroup
	workers := int(min64(int64(threads), nChunks))
	errCh := make(chan error, workers)
	abort := make(chan struct{})
	var failOnce sync.Once

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if ctx.Err() != nil {
					return
				}
				select {
				case <-abort:
					return
				default:
				}
				if err := e.fetchChunk(ctx, client, f, t.URL, idx, chunkSize, info.size, &done, &mu, &st, statePath); err != nil {
					if ctx.Err() != nil {
						return
					}
					select {
					case errCh <- err:
					default:
					}
					failOnce.Do(func() { close(abort) })
					return
				}
			}
		}()
	}
	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}
	_ = os.Remove(statePath)
	upd(func(x *task.Task) { x.FilePath = target })
	return nil
}

// fetchChunk 下载单个分块（带重试与游标续传）
func (e *Engine) fetchChunk(ctx context.Context, client *http.Client, f *os.File, raw string, idx, chunkSize, total int64, done *atomic.Int64, mu *sync.Mutex, st *chunkState, statePath string) error {
	lo := idx * chunkSize
	hi := min64(lo+chunkSize-1, total-1)
	off := lo
	for attempt := 0; off <= hi; {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		setExtraHeaders(req, raw)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, hi))
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			attempt++
			if attempt > 5 {
				return fmt.Errorf("分块 %d 下载失败: %w", idx, err)
			}
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		if resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			return fmt.Errorf("服务器不支持 Range 续传 (HTTP %d)", resp.StatusCode)
		}
		buf := make([]byte, 256<<10)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := f.WriteAt(buf[:n], off); werr != nil {
					resp.Body.Close()
					return werr
				}
				off += int64(n)
				done.Add(int64(n))
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				resp.Body.Close()
				if ctx.Err() != nil {
					return ctx.Err()
				}
				break // 内层 break 触发重试
			}
		}
		resp.Body.Close()
		if off > hi {
			break
		}
		attempt++
		if attempt > 5 {
			return fmt.Errorf("分块 %d 下载中断", idx)
		}
		select {
		case <-time.After(time.Duration(attempt) * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	mu.Lock()
	st.Done[idx] = true
	mu.Unlock()

	// 持久化断点（节流：每块一次足够轻量）
	mu.Lock()
	b, _ := json.Marshal(st)
	_ = os.WriteFile(statePath, b, 0o644)
	mu.Unlock()
	return nil
}

// ---------- 进度统计 ----------

func trackProgress(ctx context.Context, t *task.Task, upd task.Updater, done *atomic.Int64, total int64) func() {
	var mu sync.Mutex
	prev := done.Load()
	prevAt := time.Now()
	var ema float64
	done2 := time.NewTicker(500 * time.Millisecond)
	go func() {
		for {
			select {
			case <-ctx.Done():
				done2.Stop()
				return
			case <-done2.C:
				cur := done.Load()
				now := time.Now()
				mu.Lock()
				dt := now.Sub(prevAt).Seconds()
				if dt > 0.2 {
					inst := float64(cur-prev) / dt
					if ema == 0 {
						ema = inst
					} else {
						ema = ema*0.6 + inst*0.4
					}
					prev = cur
					prevAt = now
				}
				speed := ema
				mu.Unlock()
				upd(func(x *task.Task) {
					x.Done = cur
					x.Speed = speed
					if total > 0 {
						x.Progress = float64(cur) / float64(total) * 100
						if speed > 1024 {
							x.ETA = int64(float64(total-cur) / speed)
						}
					}
				})
			}
		}
	}()
	return func() { done2.Stop() }
}

// ---------- 工具 ----------

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
