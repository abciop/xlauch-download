// Package douyin 抖音视频专用解析：抖音的详情接口需要浏览器端动态签名，
// yt-dlp 的裸请求会被 WAF 拦截（报 Fresh cookies）。这里改用系统 WebView2
// 加载视频页，由浏览器自动完成验证与签名，再截取 aweme/detail 响应。
package douyin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"tdm/internal/config"
	"tdm/internal/task"
	"tdm/internal/ytdlp"
)

// IsURL 判断是否为抖音链接
func IsURL(raw string) bool {
	u := strings.ToLower(raw)
	return strings.Contains(u, "douyin.com") || strings.Contains(u, "iesdouyin.com")
}

var (
	reVideoID = regexp.MustCompile(`/(?:video|note|slides)/(\d+)`)
	reModalID = regexp.MustCompile(`modal_id=(\d+)`)
	reDigits  = regexp.MustCompile(`^\d{15,25}$`)
)

// AwemeID 从链接中提取作品 ID；短链接通过 HTTP 跳转获取。
func AwemeID(ctx context.Context, raw string) (string, error) {
	if id := matchID(raw); id != "" {
		return id, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSpace(raw), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", ua)
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("短链接解析失败: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.Request != nil && resp.Request.URL != nil {
		if id := matchID(resp.Request.URL.String()); id != "" {
			return id, nil
		}
	}
	return "", fmt.Errorf("无法识别抖音视频 ID")
}

func matchID(raw string) string {
	s := strings.TrimSpace(raw)
	if m := reVideoID.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	if m := reModalID.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	if reDigits.MatchString(s) {
		return s
	}
	return ""
}

// ---------- 详情结构 ----------

type addr struct {
	URI      string   `json:"uri"`
	URLList  []string `json:"url_list"`
	Width    int      `json:"width"`
	Height   int      `json:"height"`
}

type bitRate struct {
	GearName    string `json:"gear_name"`
	QualityType int    `json:"quality_type"`
	BitRate     int64  `json:"bit_rate"`
	PlayAddr    *addr  `json:"play_addr"`
}

type videoInfo struct {
	Duration     int64     `json:"duration"`
	PlayAddr     *addr     `json:"play_addr"`
	DownloadAddr *addr     `json:"download_addr"`
	Cover        *addr     `json:"cover"`
	BitRate      []bitRate `json:"bit_rate"`
}

type detailData struct {
	AwemeID string
	Desc    string
	Video   *videoInfo
}

type detailResp struct {
	AwemeDetail *struct {
		AwemeID string     `json:"aweme_id"`
		Desc    string     `json:"desc"`
		Video   *videoInfo `json:"video"`
	} `json:"aweme_detail"`
	FilterDetail *struct {
		DetailMsg    string `json:"detail_msg"`
		FilterReason string `json:"filter_reason"`
	} `json:"filter_detail"`
}

func parseDetail(body []byte) (*detailData, error) {
	var r detailResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("抖音详情解析失败: %w", err)
	}
	if r.AwemeDetail == nil {
		if r.FilterDetail != nil && r.FilterDetail.DetailMsg != "" {
			return nil, fmt.Errorf("抖音：%s", r.FilterDetail.DetailMsg)
		}
		return nil, fmt.Errorf("抖音未返回视频数据，可能需要登录或完成人机验证")
	}
	d := &detailData{
		AwemeID: r.AwemeDetail.AwemeID,
		Desc:    strings.TrimSpace(r.AwemeDetail.Desc),
		Video:   r.AwemeDetail.Video,
	}
	if d.Video == nil {
		return nil, fmt.Errorf("该作品没有可下载的视频数据")
	}
	return d, nil
}

// parseRouterData 解析页面内嵌的 window._ROUTER_DATA（服务端渲染数据）。
// 公开视频通常不会请求 aweme/detail，视频信息直接内嵌在路由数据里。
func parseRouterData(body []byte, fallbackID string) (*detailData, error) {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("路由数据解析失败: %w", err)
	}
	item := findVideoItem(root)
	if item == nil {
		return nil, fmt.Errorf("路由数据中未找到视频信息")
	}
	b, err := json.Marshal(item)
	if err != nil {
		return nil, err
	}
	var it struct {
		AwemeID string     `json:"aweme_id"`
		Desc    string     `json:"desc"`
		Video   *videoInfo `json:"video"`
	}
	if err := json.Unmarshal(b, &it); err != nil {
		return nil, fmt.Errorf("视频信息解析失败: %w", err)
	}
	if it.Video == nil || firstURL(it.Video.PlayAddr) == "" && firstURL(it.Video.DownloadAddr) == "" {
		return nil, fmt.Errorf("视频信息不完整")
	}
	id := it.AwemeID
	if id == "" {
		id = fallbackID
	}
	return &detailData{AwemeID: id, Desc: strings.TrimSpace(it.Desc), Video: it.Video}, nil
}

// findVideoItem 递归查找包含 video.play_addr 的作品对象。
func findVideoItem(v any) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		if raw, ok := t["video"].(map[string]any); ok {
			if _, ok := raw["play_addr"]; ok {
				return t
			}
		}
		for _, x := range t {
			if r := findVideoItem(x); r != nil {
				return r
			}
		}
	case []any:
		for _, x := range t {
			if r := findVideoItem(x); r != nil {
				return r
			}
		}
	}
	return nil
}

// Analyze 解析抖音视频，返回与 yt-dlp 一致的画质列表。
func Analyze(ctx context.Context, raw string) (*ytdlp.AnalyzeResult, error) {
	d, err := fetchDetail(ctx, raw)
	if err != nil {
		return nil, err
	}
	title := d.Desc
	if title == "" {
		title = "抖音视频 " + d.AwemeID
	}
	res := &ytdlp.AnalyzeResult{Title: title, Site: "douyin.com"}
	if cover := firstURL(d.Video.Cover); cover != "" {
		res.Thumbnail = cover
	}
	if d.Video.Duration > 0 {
		res.Duration = d.Video.Duration / 1000
	}
	for _, br := range d.Video.BitRate {
		u := firstURL(br.PlayAddr)
		if u == "" {
			continue
		}
		res.Formats = append(res.Formats, ytdlp.Format{
			FormatID:   fmt.Sprintf("br%d", br.BitRate),
			Ext:        "mp4",
			Resolution: resolutionOf(br.PlayAddr),
			Vcodec:     "h264",
			Tbr:        float64(br.BitRate) / 1000,
			Note:       br.GearName,
		})
	}
	if len(res.Formats) == 0 {
		if u := firstURL(d.Video.PlayAddr); u != "" {
			res.Formats = append(res.Formats, ytdlp.Format{
				FormatID: "best", Ext: "mp4", Resolution: resolutionOf(d.Video.PlayAddr), Vcodec: "h264",
			})
		}
	}
	if len(res.Formats) == 0 {
		return nil, fmt.Errorf("该作品没有可用的视频地址")
	}
	sort.SliceStable(res.Formats, func(i, j int) bool {
		return res.Formats[i].Tbr > res.Formats[j].Tbr
	})
	return res, nil
}

func resolutionOf(a *addr) string {
	if a == nil || a.Width <= 0 || a.Height <= 0 {
		return "best"
	}
	return fmt.Sprintf("%dx%d", a.Width, a.Height)
}

func firstURL(a *addr) string {
	if a == nil {
		return ""
	}
	for _, u := range a.URLList {
		if strings.HasPrefix(u, "http") {
			return u
		}
	}
	return ""
}

// ---------- 下载 ----------

// Engine 抖音下载引擎：解析出无水印直链后交给 HTTP 多线程下载器。
type Engine struct {
	http EngineRunner
	cfg  *config.Config
}

// EngineRunner 由 engine 包注入，避免包间循环依赖。
type EngineRunner interface {
	Run(ctx context.Context, t *task.Task, upd task.Updater) error
}

// New 创建抖音下载引擎
func New(cfg *config.Config, h EngineRunner) *Engine { return &Engine{http: h, cfg: cfg} }

// Run 实现 task.Engine
func (e *Engine) Run(ctx context.Context, t *task.Task, upd task.Updater) error {
	// 内置浏览器解析最长约 75 秒，期间无进度；给出提示避免看起来卡死
	upd(func(x *task.Task) { x.Title = "解析中…" })
	defer func() {
		// 解析失败时清除占位标题，避免错误任务永远显示「解析中…」
		upd(func(x *task.Task) {
			if x.Title == "解析中…" {
				x.Title = ""
			}
		})
	}()
	d, err := fetchDetail(ctx, t.URL)
	if err != nil {
		return err
	}
	dl, err := pick(d, t.Quality)
	if err != nil {
		return err
	}
	upd(func(x *task.Task) {
		if x.Title == "解析中…" {
			x.Title = dl.Title
			if x.Title == "" {
				x.Title = dl.Filename
			}
		}
	})
	sub := *t
	sub.Kind = task.KHTTP
	sub.URL = dl.URL
	if sub.Filename == "" {
		sub.Filename = dl.Filename
	}
	return e.http.Run(ctx, &sub, upd)
}

type download struct {
	URL      string
	Title    string
	Filename string
}

func pick(d *detailData, quality string) (*download, error) {
	v := d.Video
	var chosen *addr
	want := int64(0)
	if strings.HasPrefix(quality, "br") {
		fmt.Sscanf(quality[2:], "%d", &want)
	}
	var best *bitRate
	for i := range v.BitRate {
		br := &v.BitRate[i]
		if firstURL(br.PlayAddr) == "" {
			continue
		}
		if want > 0 && br.BitRate == want {
			best = br
			break
		}
		if best == nil || br.BitRate > best.BitRate {
			best = br
		}
	}
	if best != nil {
		chosen = best.PlayAddr
	}
	if chosen == nil {
		chosen = v.PlayAddr
	}
	u := firstURL(chosen)
	if u == "" {
		u = firstURL(v.DownloadAddr)
	}
	if u == "" {
		return nil, fmt.Errorf("未找到抖音视频直链")
	}
	name := sanitizeFilename(d.Desc)
	if name == "" {
		name = "douyin_" + d.AwemeID
	}
	if len([]rune(name)) > 90 {
		name = string([]rune(name)[:90])
	}
	if !strings.HasSuffix(strings.ToLower(name), ".mp4") {
		name += ".mp4"
	}
	return &download{URL: u, Title: d.Desc, Filename: name}, nil
}

func sanitizeFilename(s string) string {
	s = strings.TrimSpace(s)
	repl := strings.NewReplacer(
		"\\", " ", "/", " ", ":", " ", "*", " ", "?", " ", "\"", " ", "<", " ", ">", " ", "|", " ",
		"\r", " ", "\n", " ", "\t", " ",
	)
	s = repl.Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	return s
}

const ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
