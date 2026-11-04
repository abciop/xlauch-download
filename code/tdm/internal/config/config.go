// Package config 管理全局配置与数据目录（对标 TDM Fast 的 settings 体系）。
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
)

// Config 应用配置（持久化为 config.json）
type Config struct {
	DownloadDir    string `json:"downloadDir"`    // 默认下载目录
	MaxThreads     int    `json:"maxThreads"`     // HTTP 分块下载并发数
	HlsConcurrency int    `json:"hlsConcurrency"` // m3u8 分片并发数
	BtMaxPeers     int    `json:"btMaxPeers"`     // BT 单任务最大连接数
	MaxActive      int    `json:"maxActive"`      // 同时运行任务数
	Port           int    `json:"port"`           // 监听端口（被占用时自动顺延）
	Proxy          string `json:"proxy"`          // http://host:port 或 socks5://host:port
	CheckClipboard bool   `json:"checkClipboard"` // 剪贴板监听
	PreferMP4      bool   `json:"preferMp4"`      // m3u8 完成后若有 ffmpeg 则 remux 为 mp4
	CloseAction    string `json:"closeAction"`    // 关闭主窗口：tray 隐藏到托盘 / exit 直接退出
	Language       string `json:"language"`

	// 视频站点 Cookie 配置
	CookiesFromBrowser string `json:"cookiesFromBrowser"` // yt-dlp --cookies-from-browser：""/chrome/edge/firefox
	CookiesFile        string `json:"cookiesFile"`        // 手动导入的 cookies.txt 路径
}

// DataDir 数据目录：exe 同目录存在 portable 标记则便携模式，否则 %LOCALAPPDATA%\TDM
func DataDir() string {
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exe)
		if _, err := os.Stat(filepath.Join(dir, "portable.flag")); err == nil {
			return dir
		}
	}
	if runtime.GOOS == "windows" {
		if la := os.Getenv("LOCALAPPDATA"); la != "" {
			return filepath.Join(la, "TDM")
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".tdm")
}

// BinDir 外部工具目录（yt-dlp / ffmpeg）
func BinDir() string { return filepath.Join(DataDir(), "bin") }

// Default 返回默认配置
func Default() *Config {
	// 默认下载目录 d:\xdm（下载时会自动创建）；D 盘不存在的机器回退到用户「下载」目录
	dir := `d:\xdm`
	if !dirExists(`d:\`) {
		dir = filepath.Join(DataDir(), "downloads")
		if home, err := os.UserHomeDir(); err == nil && runtime.GOOS == "windows" {
			if d := filepath.Join(home, "Downloads"); dirExists(d) {
				dir = d
			}
		}
	}
	return &Config{
		DownloadDir:    dir,
		MaxThreads:     16,
		HlsConcurrency: 8,
		BtMaxPeers:     40,
		MaxActive:      3,
		Port:           50710,
		Proxy:          "",
		CheckClipboard: true,
		PreferMP4:      true,
		CloseAction:    "tray",
		Language:       "zh-CN",

		CookiesFromBrowser: "",
		CookiesFile:        "",
	}
}

// Load 读取配置，不存在则写入默认值
func Load() *Config {
	c := Default()
	p := filepath.Join(DataDir(), "config.json")
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, c)
	}
	if c.MaxThreads <= 0 {
		c.MaxThreads = 16
	}
	if c.HlsConcurrency <= 0 {
		c.HlsConcurrency = 8
	}
	if c.BtMaxPeers <= 0 {
		c.BtMaxPeers = 40
	}
	if c.MaxActive <= 0 {
		c.MaxActive = 3
	}
	if c.CloseAction != "exit" {
		c.CloseAction = "tray"
	}
	if c.Port < 1 || c.Port > 65535 {
		c.Port = 50710
	}
	switch c.CookiesFromBrowser {
	case "chrome", "edge", "firefox":
	default:
		c.CookiesFromBrowser = ""
	}
	return c
}

// Save 写回配置
func (c *Config) Save() error {
	dir := DataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(filepath.Join(dir, "config.json"), b, 0o644)
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
