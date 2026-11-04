//go:build windows

// Package ui 剪贴板监听：发现下载链接/磁力时通过通道通知（对标 TDM Fast 剪贴板识别）。
package ui

import (
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

var (
	user32           = syscall.NewLazyDLL("user32.dll")
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	openClipboard    = user32.NewProc("OpenClipboard")
	closeClipboard   = user32.NewProc("CloseClipboard")
	getClipboardData = user32.NewProc("GetClipboardData")
	globalLock       = kernel32.NewProc("GlobalLock")   // Global* 系列在 kernel32
	globalUnlock     = kernel32.NewProc("GlobalUnlock")
	lstrlenW         = kernel32.NewProc("lstrlenW")
)

const cfUnicodeText = 13

// WatchClipboard 轮询剪贴板，向 ch 推送新出现的 URL / 磁力链接
func WatchClipboard(ch chan<- string) {
	var last string
	read := func() (s string) {
		// 防御：剪贴板为辅助功能，任何 syscall 异常都不应拖垮进程
		defer func() {
			if r := recover(); r != nil {
				s = ""
			}
		}()
		r1, _, _ := openClipboard.Call(0)
		if r1 == 0 {
			return ""
		}
		defer closeClipboard.Call()
		h, _, _ := getClipboardData.Call(cfUnicodeText)
		if h == 0 {
			return ""
		}
		p, _, _ := globalLock.Call(h)
		if p == 0 {
			return ""
		}
		defer globalUnlock.Call(h)
		n, _, _ := lstrlenW.Call(p)
		if n == 0 {
			return ""
		}
		return UTF16PtrToString(p, int(n))
	}
	for {
		time.Sleep(1500 * time.Millisecond)
		s := read()
		if s == "" || s == last {
			continue
		}
		last = s
		s = strings.TrimSpace(s)
		if looksDownloadable(s) {
			select {
			case ch <- s:
			default:
			}
		}
	}
}

func looksDownloadable(s string) bool {
	if len(s) > 2048 {
		return false
	}
	l := strings.ToLower(s)
	return strings.HasPrefix(l, "magnet:") ||
		strings.HasPrefix(l, "http://") ||
		strings.HasPrefix(l, "https://")
}

// UTF16PtrToString 读取 UTF-16 内存为 Go string
func UTF16PtrToString(p uintptr, n int) string {
	if p == 0 || n <= 0 {
		return ""
	}
	src := unsafe.Slice((*uint16)(unsafe.Pointer(p)), n)
	return string(utf16.Decode(src))
}
