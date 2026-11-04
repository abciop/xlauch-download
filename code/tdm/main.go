// TDM 多功能下载器：多线程 HTTP / m3u8 / 磁力 BT / 1800+ 站点视频（yt-dlp）。
// 单文件静态编译，UI 走系统 WebView2，对标 TDM Fast（Rust+Tauri）架构。
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"tdm/internal/config"
	"tdm/internal/engine"
	"tdm/internal/server"
	"tdm/internal/task"
	"tdm/internal/ui"
)

const appTitle = "Xlauch 下载器"

func main() {
	webMode := flag.Bool("web", false, "浏览器模式（不打开原生窗口）")
	flag.Parse()

	cfg := config.Load()
	ui.SetCloseToTray(cfg.CloseAction != "exit")
	_ = os.MkdirAll(config.DataDir(), 0o755)
	_ = os.MkdirAll(cfg.DownloadDir, 0o755)

	eng := engine.New(cfg)
	mgr := task.NewManager(cfg, eng)
	defer mgr.PersistNow()

	srv := server.New(mgr)
	if err := srv.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "服务启动失败:", err)
		os.Exit(1)
	}

	// 优雅退出
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		<-c
		mgr.PersistNow()
		os.Exit(0)
	}()

	addr := srv.Addr()

	if *webMode {
		if err := ui.OpenBrowser(addr); err != nil {
			fmt.Fprintln(os.Stderr, "打开浏览器失败:", err)
		}
		fmt.Println("Xlauch 服务已启动:", addr, "（浏览器模式，Ctrl+C 退出）")
		select {} // 阻塞
	}

	// WebView2 原生窗口；失败回退浏览器
	err := ui.OpenWindow(addr, appTitle, 1180, 780)
	if err != nil {
		fmt.Fprintln(os.Stderr, "WebView2 不可用，回退浏览器:", err)
		_ = ui.OpenBrowser(addr)
		fmt.Println("Xlauch 服务已启动:", addr, "（浏览器模式，Ctrl+C 退出）")
		select {}
	}
}
