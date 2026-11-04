package server

// 浏览器扩展（插件）支持：
//   - prepare：把内嵌的扩展文件解压到本机数据目录，供浏览器「加载已解压的扩展程序」使用
//   - open：一键拉起 Chrome / Edge 的扩展管理页（chrome://extensions / edge://extensions）
//   - zip：把内嵌扩展打包为 xdm-browser-extension.zip 供手动分发
//
// 说明：受浏览器安全模型限制，第三方程序无法静默安装扩展（正规途径是应用商店上架），
// 因此 XDM 只能把安装流程压缩到最短：解压好目录 + 打开扩展页，剩余两步（开开发者模式、
// 点「加载已解压的扩展程序」）必须由用户在浏览器里完成。

import (
	"archive/zip"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"tdm/internal/config"
)

//go:embed all:webext
var webextFS embed.FS

// extDir 内嵌扩展的目标解压目录（%LOCALAPPDATA%\TDM\browser-extension）
func extDir() string { return filepath.Join(config.DataDir(), "browser-extension") }

// handleExtensionPrepare 解压内嵌扩展到本机目录，返回目录路径。
// 幂等：重复调用直接覆盖旧文件。
func (s *Server) handleExtensionPrepare(w http.ResponseWriter, r *http.Request) {
	dst := extDir()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		writeErr(w, 500, "创建目录失败: "+err.Error())
		return
	}
	root, err := fs.Sub(webextFS, "webext")
	if err != nil {
		writeErr(w, 500, "内嵌资源缺失: "+err.Error())
		return
	}
	n := 0
	err = fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, filepath.FromSlash(p)), data, 0o644)
	})
	if err != nil {
		writeErr(w, 500, "解压失败: "+err.Error())
		return
	}
	n, _ = countFiles(root)
	writeJSON(w, 200, map[string]any{"path": dst, "count": n})
}

func countFiles(root fs.FS) (int, error) {
	n := 0
	err := fs.WalkDir(root, ".", func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return err
	})
	return n, err
}

// browserCandidates 返回指定浏览器的可执行文件候选路径（Windows 常规安装位置）
func browserCandidates(name string) []string {
	pf := os.Getenv("ProgramFiles")
	pf86 := os.Getenv("PROGRAMFILES(X86)")
	la := os.Getenv("LOCALAPPDATA")
	switch name {
	case "chrome":
		return []string{
			filepath.Join(pf, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(pf86, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(la, `Google\Chrome\Application\chrome.exe`),
		}
	case "edge":
		return []string{
			filepath.Join(pf86, `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(pf, `Microsoft\Edge\Application\msedge.exe`),
		}
	}
	return nil
}

// handleExtensionOpen 拉起 Chrome / Edge 的扩展管理页。
// 传入 {"browser":"chrome"} 或 {"browser":"edge"}。
func (s *Server) handleExtensionOpen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Browser string `json:"browser"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}
	var url string
	switch req.Browser {
	case "chrome":
		url = "chrome://extensions/"
	case "edge":
		url = "edge://extensions/"
	default:
		writeErr(w, 400, "未知的浏览器类型")
		return
	}
	for _, p := range browserCandidates(req.Browser) {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := exec.Command(p, url).Start(); err != nil {
			writeErr(w, 500, "启动浏览器失败: "+err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	writeJSON(w, 404, map[string]string{"error": "未在本机找到 " + map[string]string{"chrome": "Chrome", "edge": "Microsoft Edge"}[req.Browser]})
}

// handleExtensionZip 把内嵌扩展打包为 zip 下载（与 dist 分发包同构）。
func (s *Server) handleExtensionZip(w http.ResponseWriter, r *http.Request) {
	root, err := fs.Sub(webextFS, "webext")
	if err != nil {
		writeErr(w, 500, "内嵌资源缺失")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="xdm-browser-extension.zip"`)
	zw := zip.NewWriter(w)
	defer zw.Close()
	err = fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		fw, err := zw.Create(p)
		if err != nil {
			return err
		}
		_, err = fw.Write(data)
		return err
	})
	if err != nil {
		// 响应已开始写入，只能中断连接
		return
	}
}
