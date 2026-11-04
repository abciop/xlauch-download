// Package ui WebView2 原生窗口宿主（对标 TDM Fast 的 Tauri/wry 架构）。
// WebView2 为 Windows 系统组件，无额外依赖；失败时回退默认浏览器。
package ui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/jchv/go-webview2"
	"github.com/jchv/go-webview2/webviewloader"

	"tdm/internal/config"
)

// closeToTray 关闭主窗口时是否隐藏到托盘（false 表示直接退出）
var closeToTray atomic.Bool

func init() { closeToTray.Store(true) }

// SetCloseToTray 设置关闭主窗口行为：true 隐藏到托盘，false 直接退出。
func SetCloseToTray(on bool) { closeToTray.Store(on) }

// EnsureRuntime 确保可定位 WebView2 运行时（供隐藏解析窗口复用）。
func EnsureRuntime() error { return ensureWebView2() }

// OpenWindow 打开 WebView2 原生窗口加载指定地址；返回错误表示需要回退浏览器。
func OpenWindow(addr, title string, width, height int) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("仅支持 Windows")
	}
	if err := ensureWebView2(); err != nil {
		return fmt.Errorf("WebView2 运行时不可用: %w", err)
	}
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		DataPath:  filepath.Join(config.DataDir(), "webview"),
		WindowOptions: webview2.WindowOptions{
			Title:  title,
			Width:  uint(width),
			Height: uint(height),
			Center: true,
		},
	})
	if w == nil {
		return fmt.Errorf("WebView2 初始化失败")
	}
	defer w.Destroy()
	hwnd := uintptr(w.Window())
	installWindowIcon(hwnd) // 标题栏/任务栏使用内嵌 logo
	startTray(hwnd)         // 托盘图标 + 关闭/最小化到托盘
	defer stopTray()
	w.Navigate(addr)
	w.Run()
	return nil
}

// ensureWebView2 确保可定位 WebView2 运行时。
// 某些环境 EdgeWebView 已安装但注册表 pv 缺失，loader 会判定"未安装"，导致
// 环境异步创建失败（go-webview2 库内 log.Fatal 直接闪退）。此处先用官方接口
// 探测，失败则扫描常见安装目录，并通过官方覆盖变量 WEBVIEW2_BROWSER_EXECUTABLE_FOLDER 修复。
func ensureWebView2() error {
	if os.Getenv("WEBVIEW2_BROWSER_EXECUTABLE_FOLDER") != "" {
		return nil // 已显式指定，尊重用户配置
	}
	if v, err := webviewloader.GetInstalledVersion(); err == nil && v != "" {
		return nil // 官方定位成功
	}
	// 官方定位失败：扫描常见 Evergreen 安装位置（版本目录即 browser folder）
	roots := []string{os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramFiles"), os.Getenv("LocalAppData")}
	best := ""
	var bestParts []int
	for _, root := range roots {
		if root == "" {
			continue
		}
		app := filepath.Join(root, "Microsoft", "EdgeWebView", "Application")
		ents, err := os.ReadDir(app)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			parts, ok := verParts(e.Name())
			if !ok {
				continue
			}
			// 必须实际包含 runtime 主程序
			if _, err := os.Stat(filepath.Join(app, e.Name(), "msedgewebview2.exe")); err != nil {
				continue
			}
			if len(bestParts) == 0 || cmpVer(parts, bestParts) > 0 {
				best, bestParts = filepath.Join(app, e.Name()), parts
			}
		}
	}
	if best == "" {
		return fmt.Errorf("未找到 WebView2 运行时（注册表与安装目录均无）")
	}
	return os.Setenv("WEBVIEW2_BROWSER_EXECUTABLE_FOLDER", best)
}

// verParts 解析 "131.0.2903.86" 为数字段
func verParts(s string) ([]int, bool) {
	segs := strings.Split(s, ".")
	if len(segs) < 2 {
		return nil, false
	}
	out := make([]int, len(segs))
	for i, seg := range segs {
		n, err := strconv.Atoi(seg)
		if err != nil {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}

// cmpVer 比较版本段：1 表示 a>b，-1 表示 a<b，0 相等
func cmpVer(a, b []int) int {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x > y {
			return 1
		}
		if x < y {
			return -1
		}
	}
	return 0
}

// OpenBrowser 用系统默认浏览器打开（降级方案）
func OpenBrowser(addr string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", addr).Start()
}
