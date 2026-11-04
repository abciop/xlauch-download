// Package ytdlp 集成 yt-dlp sidecar（对标 TDM Fast 逆向结果）：
// 按需下载 yt-dlp_win.zip / ffmpeg 构建，-J 解析格式，--newline 下载并解析进度。
// yt-dlp 支持 1800+ 站点（含 YouTube/Bilibili/TikTok 等），最高 4K。
package ytdlp

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"tdm/internal/config"
	"tdm/internal/task"
)

const (
	ytdlpZipURL  = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_win.zip"
	ffmpegZipURL = "https://github.com/yt-dlp/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-win64-gpl.zip"
	ua           = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

// 进度行正则（逆向 TDM Fast 得到）
var progressRe = regexp.MustCompile(`\[download\]\s+([\d.]+)%\s+of\s+~?\s*([\d.]+)([A-Za-z]+)(?:\s+at\s+([\d.]+)([A-Za-z]+)/s)?`)
var destRe = regexp.MustCompile(`\[download\] Destination: (.+)$`)
var mergeRe = regexp.MustCompile(`Merging formats into "(.+)"`)
var alreadyRe = regexp.MustCompile(`\[download\] (.+) has already been downloaded`)

// Engine yt-dlp 视频下载引擎
type Engine struct {
	cfg *config.Config
}

// New 创建引擎
func New(cfg *config.Config) *Engine { return &Engine{cfg: cfg} }

// ---------- sidecar 管理 ----------

// YtDlpPath 返回 yt-dlp.exe 路径（不存在返回空）
func YtDlpPath() string {
	p := filepath.Join(config.BinDir(), "yt-dlp.exe")
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return p
	}
	return ""
}

// FfmpegPath 返回 ffmpeg.exe 路径（bin 或 PATH）
func FfmpegPath() string {
	p := filepath.Join(config.BinDir(), "ffmpeg.exe")
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return p
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return ""
}

// ToolsStatus 工具状态
type ToolsStatus struct {
	YtdlpPresent bool   `json:"ytdlpPresent"`
	YtdlpVersion string `json:"ytdlpVersion"`
	FfmpegPresent bool  `json:"ffmpegPresent"`
	FfmpegVersion string `json:"ffmpegVersion"`
}

// probe 运行 yt-dlp --version，确认二进制真正可用。
// 官方 yt-dlp_win.zip 为 PyInstaller onedir 布局，必须同时存在 _internal 目录，
// 只拷贝 yt-dlp.exe 会报 "Failed to load Python DLL"。
func probe() (string, bool) {
	p := YtDlpPath()
	if p == "" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p, "--version")
	cmd.SysProcAttr = hideWindow()
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", false
	}
	return v, true
}

// Status 探测工具版本
func Status() ToolsStatus {
	st := ToolsStatus{}
	if v, ok := probe(); ok {
		st.YtdlpPresent = true
		st.YtdlpVersion = v
	}
	if p := FfmpegPath(); p != "" {
		st.FfmpegPresent = true
		out, err := exec.Command(p, "-version").Output()
		if err == nil {
			if line := strings.SplitN(string(out), "\n", 2); len(line) > 0 {
				st.FfmpegVersion = strings.TrimSpace(line[0])
			}
		}
	}
	return st
}

var ensureMu sync.Mutex

// EnsureYtDlp 确保 yt-dlp 可运行，缺失或残缺则重新下载（onedir 整包解压）
func EnsureYtDlp(ctx context.Context) error {
	if _, ok := probe(); ok {
		return nil
	}
	ensureMu.Lock()
	defer ensureMu.Unlock()
	if _, ok := probe(); ok { // 等锁期间可能已被其他请求修好
		return nil
	}
	bin := config.BinDir()
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	// 清理残缺安装（旧的 exe 与 _internal 一并删除后整包解压）
	os.Remove(filepath.Join(bin, "yt-dlp.exe"))
	os.RemoveAll(filepath.Join(bin, "_internal"))
	zipPath := filepath.Join(bin, "yt-dlp_win.zip")
	if err := downloadFile(ctx, ytdlpZipURL, zipPath); err != nil {
		return fmt.Errorf("下载 yt-dlp 失败: %w", err)
	}
	defer os.Remove(zipPath)
	if err := extractZipAll(zipPath, bin); err != nil {
		return fmt.Errorf("解压 yt-dlp 失败: %w", err)
	}
	if _, ok := probe(); !ok {
		return fmt.Errorf("yt-dlp.exe 无法运行（安装包不完整）")
	}
	return nil
}

// EnsureFfmpeg 下载 ffmpeg（体积大，按需调用）
func EnsureFfmpeg(ctx context.Context) error {
	if FfmpegPath() != "" {
		return nil
	}
	bin := config.BinDir()
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	zipPath := filepath.Join(bin, "ffmpeg.zip")
	if err := downloadFile(ctx, ffmpegZipURL, zipPath); err != nil {
		return fmt.Errorf("下载 ffmpeg 失败: %w", err)
	}
	defer os.Remove(zipPath)
	if err := extractZip(zipPath, bin, "ffmpeg.exe", "ffprobe.exe"); err != nil {
		return fmt.Errorf("解压 ffmpeg 失败: %w", err)
	}
	if FfmpegPath() == "" {
		return fmt.Errorf("ffmpeg.exe 未就绪")
	}
	return nil
}

func downloadFile(ctx context.Context, url, dest string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", ua)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// extractZip 解压 zip 中指定文件名（顶层或任意层级）到目标目录
func extractZip(zipPath, dest string, names ...string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	for _, f := range r.File {
		base := filepath.Base(f.Name)
		if !want[base] || f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		out, err := os.Create(filepath.Join(dest, base))
		if err != nil {
			rc.Close()
			return err
		}
		_, cpErr := io.Copy(out, rc)
		out.Close()
		rc.Close()
		if cpErr != nil {
			return cpErr
		}
		delete(want, base)
		if len(want) == 0 {
			return nil
		}
	}
	if len(want) > 0 {
		return fmt.Errorf("压缩包缺少 %v", names)
	}
	return nil
}

// extractZipAll 解压整个 zip 到 dest（保留相对路径，含 _internal 等子目录）
func extractZipAll(zipPath, dest string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	root := filepath.Clean(dest)
	for _, f := range r.File {
		target := filepath.Join(root, filepath.FromSlash(f.Name))
		rel, err := filepath.Rel(root, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			continue // 防目录穿越
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(target, 0o755)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			rc.Close()
			return err
		}
		_, cpErr := io.Copy(out, rc)
		out.Close()
		rc.Close()
		if cpErr != nil {
			return cpErr
		}
	}
	return nil
}

// ---------- 格式分析 ----------

// Format 可选画质
type Format struct {
	FormatID  string `json:"formatId"`
	Ext       string `json:"ext"`
	Resolution string `json:"resolution"`
	Fps       int    `json:"fps,omitempty"`
	Vcodec    string `json:"vcodec"`
	Acodec    string `json:"acodec"`
	Filesize  int64  `json:"filesize,omitempty"`
	Tbr       float64 `json:"tbr,omitempty"`
	Note      string `json:"note,omitempty"`
}

// AnalyzeResult 站点视频分析结果
type AnalyzeResult struct {
	Title     string   `json:"title"`
	Thumbnail string   `json:"thumbnail,omitempty"`
	Duration  int64    `json:"duration,omitempty"`
	Site      string   `json:"site,omitempty"`
	Formats   []Format `json:"formats"`
}

// Analyze 用 yt-dlp -J 解析视频信息（对标逆向参数 -J --no-warnings --no-playlist）。
// cfg 用于透传 Cookie / 代理配置，降低站点风控拦截概率。
func Analyze(ctx context.Context, cfg *config.Config, url string) (*AnalyzeResult, error) {
	if err := EnsureYtDlp(ctx); err != nil {
		return nil, err
	}
	p := YtDlpPath()
	args := []string{"-J", "--no-warnings", "--no-playlist", "--no-check-certificates"}
	if cfg != nil {
		args = append(args, CookieArgs(cfg, url)...)
		if cfg.Proxy != "" {
			args = append(args, "--proxy", cfg.Proxy)
		}
	}
	args = append(args, url)
	cmd := exec.CommandContext(ctx, p, args...)
	cmd.SysProcAttr = hideWindow()
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			msg := strings.TrimSpace(string(ee.Stderr))
			if msg == "" {
				msg = string(out)
			}
			return nil, fmt.Errorf("yt-dlp 解析失败: %s", truncStr(msg, 400))
		}
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("解析 yt-dlp JSON 失败: %w", err)
	}
	res := &AnalyzeResult{
		Title: str(raw, "title"),
		Thumbnail: str(raw, "thumbnail"),
		Duration: num(raw, "duration"),
	}
	if u := str(raw, "webpage_url"); u != "" {
		if i := strings.Index(u, "://"); i > 0 {
			if j := strings.Index(u[i+3:], "/"); j > 0 {
				res.Site = u[i+3 : i+3+j]
			} else {
				res.Site = u[i+3:]
			}
		}
	}
	seen := map[string]bool{}
	if fmts, ok := raw["formats"].([]any); ok {
		for _, f := range fmts {
			fm, ok := f.(map[string]any)
			if !ok {
				continue
			}
			fo := Format{
				FormatID:  str(fm, "format_id"),
				Ext:       str(fm, "ext"),
				Resolution: str(fm, "resolution"),
				Vcodec:    str(fm, "vcodec"),
				Acodec:    str(fm, "acodec"),
				Filesize:  num(fm, "filesize"),
				Tbr:       numFloat(fm, "tbr"),
				Note:      str(fm, "format_note"),
			}
			fo.Fps = int(num(fm, "fps"))
			// 只保留视频行（含分辨率），去重
			if fo.Resolution == "" || fo.Resolution == "audio only" {
				continue
			}
			key := fo.Resolution + "|" + fmt.Sprint(fo.Fps)
			if seen[key] {
				continue
			}
			seen[key] = true
			res.Formats = append(res.Formats, fo)
		}
	}
	if len(res.Formats) == 0 {
		// 无详细格式时给默认 best
		res.Formats = []Format{{FormatID: "bv*+ba/b", Resolution: "best", Note: "最佳画质"}}
	}
	return res, nil
}

// ---------- 下载引擎 ----------

// Run 阻塞执行视频下载
func (e *Engine) Run(ctx context.Context, t *task.Task, upd task.Updater) error {
	if err := EnsureYtDlp(ctx); err != nil {
		return err
	}
	if err := os.MkdirAll(t.Dir, 0o755); err != nil {
		return err
	}

	// yt-dlp 先解析再下载：解析阶段无进度输出，给出"解析中"提示避免看起来卡死
	upd(func(x *task.Task) { x.Title = "解析中…" })
	defer func() {
		// 解析/下载失败时清除占位标题，避免错误任务永远显示「解析中…」
		upd(func(x *task.Task) {
			if x.Title == "解析中…" {
				x.Title = ""
			}
		})
	}()

	// 画质选择：quality 为 format_id 时组合最佳音轨；特殊值直用
	fs := t.Quality
	switch fs {
	case "", "best":
		fs = "bv*+ba/b"
	case "audio":
		fs = "ba/b"
	default:
		fs = fs + "+ba/b/" + fs
	}

	outTpl := "%(title)s.%(ext)s"
	if t.Filename != "" {
		name := strings.TrimSuffix(t.Filename, filepath.Ext(t.Filename))
		outTpl = name + ".%(ext)s"
	}

	args := []string{
		"-f", fs,
		"--newline",
		"--no-playlist",
		"--no-warnings",
		"--no-check-certificates",
		"--retries", "10",
		"--fragment-retries", "10",
		"--retry-sleep", "exp=1:30",
		"--concurrent-fragments", fmt.Sprint(e.cfg.HlsConcurrency),
		"--no-mtime",
		"-o", filepath.Join(t.Dir, outTpl),
	}
	if p := FfmpegPath(); p != "" {
		args = append(args, "--ffmpeg-location", filepath.Dir(p))
	}
	if e.cfg.Proxy != "" {
		args = append(args, "--proxy", e.cfg.Proxy)
	}
	args = append(args, CookieArgs(e.cfg, t.URL)...)
	args = append(args, t.URL)

	cmd := exec.CommandContext(ctx, YtDlpPath(), args...)
	cmd.SysProcAttr = hideWindow()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 yt-dlp 失败: %w", err)
	}

	var filePath string
	unitMap := map[string]float64{"B": 1, "KiB": 1024, "MiB": 1024 * 1024, "GiB": 1024 * 1024 * 1024, "TiB": 1024 * 1024 * 1024 * 1024, "KB": 1000, "MB": 1000 * 1000, "GB": 1000 * 1000 * 1000}

	// 记录目标文件：尽早落位 FilePath（删除任务可定位），并把"解析中…"替换为真实标题
	noteDest := func(p string) {
		filePath = p
		upd(func(x *task.Task) {
			x.FilePath = filePath
			if x.Title == "解析中…" {
				x.Title = strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
			}
		})
	}

	buf := make([]byte, 64*1024)
	var line []byte
	lastUpd := time.Now()
	for {
		n, rerr := stdout.Read(buf)
		if n > 0 {
			line = append(line, buf[:n]...)
			for {
				i := bytes.IndexByte(line, '\n')
				if i < 0 {
					break
				}
				ln := strings.TrimSpace(string(line[:i]))
				line = line[i+1:]
				if ln == "" {
					continue
				}
				// 目标文件路径（尽早落位，删除任务时可定位文件）
				if m := destRe.FindStringSubmatch(ln); m != nil {
					noteDest(m[1])
				}
				if m := mergeRe.FindStringSubmatch(ln); m != nil {
					noteDest(m[1])
				}
				if m := alreadyRe.FindStringSubmatch(ln); m != nil {
					noteDest(m[1])
				}
				// 进度
				if m := progressRe.FindStringSubmatch(ln); m != nil {
					pct := parseFloat(m[1])
					size := parseFloat(m[2]) * unitMap[m[3]]
					var spd float64
					if m[4] != "" {
						spd = parseFloat(m[4]) * unitMap[m[5]]
					}
					if time.Since(lastUpd) > 300*time.Millisecond {
						lastUpd = time.Now()
						upd(func(x *task.Task) {
							if size > 0 {
								x.Total = int64(size)
								x.Done = int64(size * pct / 100)
							}
							x.Progress = pct
							if spd > 0 {
								x.Speed = spd
							}
						})
					}
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	_ = cmd.Wait()

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		return fmt.Errorf("yt-dlp 退出码 %d", code)
	}
	if filePath != "" {
		if fi, err := os.Stat(filePath); err == nil {
			upd(func(x *task.Task) {
				x.FilePath = filePath
				x.Total = fi.Size()
				x.Done = fi.Size()
			})
		}
	}
	return nil
}

// ---------- 辅助 ----------

// hideWindow 子进程完全不创建控制台窗口（GUI 进程启动控制台程序时必需）
func hideWindow() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

func truncStr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func str(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}

func num(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

func numFloat(m map[string]any, k string) float64 {
	v, _ := m[k].(float64)
	return v
}

func parseFloat(s string) float64 {
	var f float64
	fmt.Sscanf(s, "%g", &f)
	return f
}
