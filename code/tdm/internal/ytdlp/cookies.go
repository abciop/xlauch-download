package ytdlp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"tdm/internal/config"
)

// CookieEntry 单条 Cookie（浏览器扩展推送 / 注入 WebView2 共用的结构）。
type CookieEntry struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"` // unix 秒；<=0 表示会话 Cookie
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
}

// CookieDir Cookie 存储目录：<DataDir>\cookies
func CookieDir() string { return filepath.Join(config.DataDir(), "cookies") }

var reSite = regexp.MustCompile(`^[a-z0-9][a-z0-9.\-]{0,63}$`)

// normalizeSite 校验并规范化站点标识（如 douyin.com / youtube.com）。
func normalizeSite(site string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(site, "https://"), "http://")))
	s = strings.SplitN(s, "/", 2)[0]
	s = strings.TrimPrefix(s, "www.")
	if !reSite.MatchString(s) {
		return "", fmt.Errorf("站点标识无效: %q", site)
	}
	return s, nil
}

// SaveSiteCookies 保存某站点 Cookie：{site}.json（原始结构）+ {site}.txt（Netscape 格式，供 yt-dlp --cookies 使用）。
func SaveSiteCookies(site string, cookies []CookieEntry) (string, int, error) {
	s, err := normalizeSite(site)
	if err != nil {
		return "", 0, err
	}
	if len(cookies) == 0 {
		return "", 0, fmt.Errorf("Cookie 列表为空")
	}
	if len(cookies) > 2000 {
		cookies = cookies[:2000]
	}
	// 逐条校验并跳过无效项：RFC 6265 允许空名 Cookie（如 "=delete" 清除标记），
	// 抖音等站点的 CDN 会下发此类条目 —— 混入批次时应跳过而不是整批拒绝。
	valid := make([]CookieEntry, 0, len(cookies))
	for i := range cookies {
		c := cookies[i]
		c.Name = strings.TrimSpace(c.Name)
		c.Value = strings.TrimSpace(c.Value)
		if c.Name == "" || len(c.Name) > 256 || len(c.Value) > 4096 {
			continue
		}
		if c.Path == "" {
			c.Path = "/"
		}
		valid = append(valid, c)
	}
	cookies = valid
	if len(cookies) == 0 {
		return "", 0, fmt.Errorf("没有有效的 Cookie")
	}
	if err := os.MkdirAll(CookieDir(), 0o755); err != nil {
		return "", 0, err
	}
	jb, err := json.MarshalIndent(cookies, "", " ")
	if err != nil {
		return "", 0, err
	}
	if err := os.WriteFile(filepath.Join(CookieDir(), s+".json"), jb, 0o644); err != nil {
		return "", 0, err
	}
	if err := os.WriteFile(filepath.Join(CookieDir(), s+".txt"), []byte(toNetscape(cookies)), 0o644); err != nil {
		return "", 0, err
	}
	return s, len(cookies), nil
}

// LoadSiteCookies 读取站点 Cookie（douyin WebView2 注入用）。
func LoadSiteCookies(site string) []CookieEntry {
	s, err := normalizeSite(site)
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(CookieDir(), s+".json"))
	if err != nil {
		return nil
	}
	var list []CookieEntry
	if json.Unmarshal(b, &list) != nil {
		return nil
	}
	return list
}

// ClearSiteCookies 删除某站点已导入的 Cookie。
func ClearSiteCookies(site string) error {
	s, err := normalizeSite(site)
	if err != nil {
		return err
	}
	os.Remove(filepath.Join(CookieDir(), s+".json"))
	os.Remove(filepath.Join(CookieDir(), s+".txt"))
	return nil
}

// SiteCookieStatus 已导入站点状态（设置页展示用）。
type SiteCookieStatus struct {
	Site      string `json:"site"`
	Count     int    `json:"count"`
	UpdatedAt string `json:"updatedAt"`
}

// ListSiteCookies 扫描已导入的站点。
func ListSiteCookies() []SiteCookieStatus {
	ents, err := os.ReadDir(CookieDir())
	if err != nil {
		return nil
	}
	var out []SiteCookieStatus
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		site := strings.TrimSuffix(name, ".json")
		b, err := os.ReadFile(filepath.Join(CookieDir(), name))
		if err != nil {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		var list []CookieEntry
		_ = json.Unmarshal(b, &list)
		out = append(out, SiteCookieStatus{
			Site:      site,
			Count:     len(list),
			UpdatedAt: fi.ModTime().Format("2006-01-02 15:04"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out
}

// toNetscape 转为 Netscape cookies.txt 格式（curl / yt-dlp 通用）。
// 每行：domain \t includeSubdomains \t path \t secure \t expiry \t name \t value
func toNetscape(cookies []CookieEntry) string {
	var b strings.Builder
	b.WriteString("# Netscape HTTP Cookie File\n# https://curl.se/docs/http-cookies.html\n\n")
	for _, c := range cookies {
		domain := strings.TrimSpace(c.Domain)
		if domain == "" {
			continue
		}
		includeSub := "FALSE"
		if strings.HasPrefix(domain, ".") {
			includeSub = "TRUE"
		}
		expiry := int64(c.Expires)
		if expiry <= 0 {
			expiry = 0 // 会话 Cookie；yt-dlp 视为会话
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			domain, includeSub, c.Path, boolStr(c.Secure), expiry, c.Name, c.Value)
	}
	return b.String()
}

func boolStr(b bool) string {
	if b {
		return "TRUE"
	}
	return "FALSE"
}

// CookieArgs 为 yt-dlp 生成 Cookie 参数（优先级：站点导入 > cookies.txt > 读取浏览器）。
func CookieArgs(cfg *config.Config, pageURL string) []string {
	if f := matchSiteCookieFile(pageURL); f != "" {
		return []string{"--cookies", f}
	}
	if cfg.CookiesFile != "" {
		if fi, err := os.Stat(cfg.CookiesFile); err == nil && !fi.IsDir() {
			return []string{"--cookies", cfg.CookiesFile}
		}
	}
	switch cfg.CookiesFromBrowser {
	case "chrome", "edge", "firefox":
		return []string{"--cookies-from-browser", cfg.CookiesFromBrowser}
	}
	return nil
}

// matchSiteCookieFile 查找与 URL 主机匹配的站点 Cookie 文件（最长后缀优先）。
func matchSiteCookieFile(pageURL string) string {
	u, err := url.Parse(strings.TrimSpace(pageURL))
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	ents, err := os.ReadDir(CookieDir())
	if err != nil {
		return ""
	}
	best := ""
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".txt") {
			continue
		}
		site := strings.TrimSuffix(name, ".txt")
		if site == host || strings.HasSuffix(host, "."+site) {
			if len(site) > len(best) {
				best = site
			}
		}
	}
	if best == "" {
		return ""
	}
	return filepath.Join(CookieDir(), best+".txt")
}
