// Package server 提供内嵌 Web UI 与 REST/SSE API。
package server

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"tdm/internal/config"
	"tdm/internal/douyin"
	"tdm/internal/task"
	"tdm/internal/ui"
	"tdm/internal/ytdlp"
)

//go:embed all:web
var webFS embed.FS

// Server HTTP 服务
type Server struct {
	mgr    *task.Manager
	ln     net.Listener
	clipCh chan string
}

// New 创建服务（传入已就绪的任务管理器）
func New(mgr *task.Manager) *Server {
	s := &Server{mgr: mgr, clipCh: make(chan string, 8)}
	if mgr.Config().CheckClipboard {
		go ui.WatchClipboard(s.clipCh)
	}
	return s
}

// Addr 服务地址（Start 后有效）
func (s *Server) Addr() string { return "http://" + s.ln.Addr().String() }

// Start 监听配置端口（默认 50710，被占用时自动顺延）
func (s *Server) Start() error {
	ln, err := listenPreferred(s.mgr.Config().Port)
	if err != nil {
		return err
	}
	s.ln = ln
	mux := http.NewServeMux()
	s.routes(mux)
	go http.Serve(ln, mux)
	return nil
}

// listenPreferred 优先使用配置端口；被占用时依次尝试后续端口，仍失败则回退随机端口。
func listenPreferred(port int) (net.Listener, error) {
	if port < 1 || port > 65535 {
		port = 50710
	}
	for p := port; p <= 65535 && p < port+100; p++ {
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p)); err == nil {
			return ln, nil
		}
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

func (s *Server) routes(mux *http.ServeMux) {
	// 静态资源
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))

	// API
	mux.HandleFunc("GET /api/tasks", s.handleTasks)
	mux.HandleFunc("POST /api/tasks", s.handleAddTask)
	mux.HandleFunc("POST /api/tasks/{id}/action", s.handleAction)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/config", s.handleGetConfig)
	mux.HandleFunc("POST /api/config", s.handleSetConfig)
	mux.HandleFunc("POST /api/analyze", s.handleAnalyze)
	mux.HandleFunc("GET /api/tools", s.handleTools)
	mux.HandleFunc("POST /api/tools/ytdlp", s.handleToolYtdlp)
	mux.HandleFunc("POST /api/tools/ffmpeg", s.handleToolFfmpeg)
	mux.HandleFunc("GET /api/cookies", s.handleListCookies)
	mux.HandleFunc("POST /api/cookies/import", s.handleImportCookies)
	mux.HandleFunc("POST /api/cookies/clear", s.handleClearCookies)
	mux.HandleFunc("POST /api/config/reset", s.handleResetConfig)
	mux.HandleFunc("POST /api/pick-file", s.handlePickFile)
	mux.HandleFunc("POST /api/open", s.handleOpen)
	mux.HandleFunc("POST /api/open-browser", s.handleOpenBrowser)
	mux.HandleFunc("POST /api/open-url", s.handleOpenURL)
	mux.HandleFunc("POST /api/pick-dir", s.handlePickDir)
	mux.HandleFunc("POST /api/torrent", s.handleTorrentUpload)
	mux.HandleFunc("POST /api/extension/prepare", s.handleExtensionPrepare)
	mux.HandleFunc("POST /api/extension/open", s.handleExtensionOpen)
	mux.HandleFunc("GET /api/extension/zip", s.handleExtensionZip)
	mux.HandleFunc("POST /api/quit", s.handleQuit)
}

// ---------- 任务 ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"tasks": s.mgr.Snapshot()})
}

var (
	reMagnet = regexp.MustCompile(`magnet:\?[^\s"'<>]+`)
	reHTTP   = regexp.MustCompile("https?://[^\\s\"'<>`]+")
)

// normalizeURL 从分享文案（抖音/快手/B站等 App 复制内容）中提取真实链接。
func normalizeURL(raw string) string {
	s := strings.TrimSpace(raw)
	if m := reMagnet.FindString(s); m != "" {
		return m
	}
	if m := reHTTP.FindString(s); m != "" {
		return strings.TrimRight(m, "）】」，。；、！？")
	}
	return s
}

func (s *Server) handleAddTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL      string `json:"url"`
		Dir      string `json:"dir"`
		Filename string `json:"filename"`
		Quality  string `json:"quality"`
		Kind     string `json:"kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.URL) == "" {
		writeErr(w, 400, "参数错误")
		return
	}
	t := s.mgr.Add(normalizeURL(req.URL), req.Dir, req.Filename, req.Quality, task.Kind(req.Kind))
	writeJSON(w, 200, map[string]any{"task": t})
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Action   string `json:"action"`
		DelFiles bool   `json:"delFiles"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}
	switch req.Action {
	case "pause":
		s.mgr.Pause(id)
	case "resume":
		s.mgr.Resume(id)
	case "retry":
		s.mgr.Retry(id)
	case "remove":
		s.mgr.Remove(id, req.DelFiles)
	case "openDir":
		if t, ok := s.mgr.Get(id); ok {
			p := t.Dir
			if t.FilePath != "" {
				p = filepath.Dir(t.FilePath)
			}
			_ = exec.Command("explorer", p).Start()
		}
	default:
		writeErr(w, 400, "未知操作")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- SSE ----------

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "不支持流式响应")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	send := func() {
		b, _ := json.Marshal(s.mgr.Snapshot())
		fmt.Fprintf(w, "event: tasks\ndata: %s\n\n", b)
		fl.Flush()
	}
	send() // 初始快照
	notify := s.mgr.Notify()
	hb := time.NewTicker(15 * time.Second)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-notify:
			send()
		case txt := <-s.clipCh:
			b, _ := json.Marshal(map[string]string{"text": txt})
			fmt.Fprintf(w, "event: clipboard\ndata: %s\n\n", b)
			fl.Flush()
		case <-hb.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

// ---------- 配置 ----------

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.mgr.Config())
}

func (s *Server) handleSetConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.mgr.Config()
	if err := json.NewDecoder(r.Body).Decode(cfg); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}
	if err := cfg.Save(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	ui.SetCloseToTray(cfg.CloseAction != "exit")
	writeJSON(w, 200, cfg)
}

// ---------- 站点视频分析 ----------

func (s *Server) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	var req struct{ URL string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.URL) == "" {
		writeErr(w, 400, "参数错误")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	u := normalizeURL(req.URL)
	var (
		res *ytdlp.AnalyzeResult
		err error
	)
	if douyin.IsURL(u) {
		res, err = douyin.Analyze(ctx, u)
	} else {
		res, err = ytdlp.Analyze(ctx, s.mgr.Config(), u)
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, res)
}

// ---------- 工具管理 ----------

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, ytdlp.Status())
}

func (s *Server) handleToolYtdlp(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	if err := ytdlp.EnsureYtDlp(ctx); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, ytdlp.Status())
}

func (s *Server) handleToolFfmpeg(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	if err := ytdlp.EnsureFfmpeg(ctx); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, ytdlp.Status())
}

// ---------- 本地打开 ----------

// 在系统默认浏览器中打开本体页面（WebView2 内的链接点击由此转发，确保跳到外部浏览器）
func (s *Server) handleOpenBrowser(w http.ResponseWriter, r *http.Request) {
	if err := ui.OpenBrowser(s.Addr()); err != nil {
		writeErr(w, 500, "打开浏览器失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "addr": s.Addr()})
}

func (s *Server) handleOpenURL(w http.ResponseWriter, r *http.Request) {
	var req struct{ URL string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.URL) == "" {
		writeErr(w, 400, "参数错误")
		return
	}
	u := strings.TrimSpace(req.URL)
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		writeErr(w, 400, "仅支持 http/https 链接")
		return
	}
	if err := ui.OpenBrowser(u); err != nil {
		writeErr(w, 500, "打开链接失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// 退出程序（先回包再退出，确保前端能拿到响应）
func (s *Server) handleQuit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true})
	go func() {
		time.Sleep(200 * time.Millisecond)
		ui.Quit()
	}()
}

// ---------- 种子文件导入（拖拽 .torrent 上传） ----------

// handleTorrentUpload 接收前端拖拽上传的种子文件（base64），
// 落盘到数据目录后以文件路径创建 BT 任务。
func (s *Server) handleTorrentUpload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Data     string `json:"data"`
		Filename string `json:"filename"`
		Dir      string `json:"dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}
	raw, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil || len(raw) == 0 {
		writeErr(w, 400, "种子内容无效")
		return
	}
	if len(raw) > 64<<20 {
		writeErr(w, 400, "种子文件过大（上限 64MB）")
		return
	}
	// 文件名安全化，防止路径穿越
	name := filepath.Base(req.Filename)
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "seed.torrent"
	}
	name = strings.Map(func(c rune) rune {
		switch c {
		case '\\', '/', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		return c
	}, name)
	if !strings.HasSuffix(strings.ToLower(name), ".torrent") {
		name += ".torrent"
	}
	dir := filepath.Join(config.DataDir(), "torrents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, 500, "创建种子目录失败: "+err.Error())
		return
	}
	// 时间戳前缀避免同名种子覆盖，任务标题单独传入保持原名
	path := filepath.Join(dir, fmt.Sprintf("%d-%s", time.Now().UnixMilli(), name))
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		writeErr(w, 500, "保存种子失败: "+err.Error())
		return
	}
	title := strings.TrimSuffix(name, filepath.Ext(name))
	t := s.mgr.Add(path, req.Dir, title, "", "")
	writeJSON(w, 200, map[string]any{"task": t})
}

func (s *Server) handleOpen(w http.ResponseWriter, r *http.Request) {
	var req struct{ Path string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		writeErr(w, 400, "参数错误")
		return
	}
	if fi, err := os.Stat(req.Path); err == nil && fi.IsDir() {
		_ = exec.Command("explorer", req.Path).Start()
	} else {
		_ = exec.Command("explorer", "/select,", req.Path).Start()
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// 弹出系统文件夹选择框，用于设置下载目录
func (s *Server) handlePickDir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Initial string `json:"initial"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	path, err := ui.PickDirectory(req.Initial)
	if err != nil {
		writeErr(w, 500, "打开目录选择失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"path": path, "cancel": path == ""})
}

// ---------- Cookie 管理（浏览器扩展 / cookies.txt 导入） ----------

// handleImportCookies 接收浏览器扩展推送的站点 Cookie：
// {site:"douyin.com", cookies:[{name,value,domain,path,expires,httpOnly,secure}]}
// 保存为 {site}.json（原始结构）+ {site}.txt（Netscape 格式，供 yt-dlp 使用）。
func (s *Server) handleImportCookies(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Site    string              `json:"site"`
		Cookies []ytdlp.CookieEntry `json:"cookies"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}
	site, count, err := ytdlp.SaveSiteCookies(req.Site, req.Cookies)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"site": site, "count": count})
}

// handleListCookies 已导入站点状态 + cookies.txt 配置状态（设置页展示用）。
func (s *Server) handleListCookies(w http.ResponseWriter, r *http.Request) {
	cfg := s.mgr.Config()
	valid := false
	if cfg.CookiesFile != "" {
		if fi, err := os.Stat(cfg.CookiesFile); err == nil && !fi.IsDir() {
			valid = true
		}
	}
	writeJSON(w, 200, map[string]any{
		"sites":           ytdlp.ListSiteCookies(),
		"cookiesFile":     cfg.CookiesFile,
		"cookiesFileValid": valid,
	})
}

// handleClearCookies 删除某站点已导入的 Cookie：{site:"douyin.com"}
func (s *Server) handleClearCookies(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Site string `json:"site"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Site) == "" {
		writeErr(w, 400, "参数错误")
		return
	}
	if err := ytdlp.ClearSiteCookies(req.Site); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleResetConfig 恢复默认配置并立即生效。
func (s *Server) handleResetConfig(w http.ResponseWriter, r *http.Request) {
	def := config.Default()
	if err := def.Save(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	*s.mgr.Config() = *def
	ui.SetCloseToTray(def.CloseAction != "exit")
	writeJSON(w, 200, def)
}

// 弹出系统文件选择框（cookies.txt 导入）
func (s *Server) handlePickFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Initial string `json:"initial"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	path, err := ui.PickFile(req.Initial)
	if err != nil {
		writeErr(w, 500, "打开文件选择失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"path": path, "cancel": path == ""})
}
