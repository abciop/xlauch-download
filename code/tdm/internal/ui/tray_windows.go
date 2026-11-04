//go:build windows

package ui

import (
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

// ---------- Win32 常量 ----------

const (
	wmSetIcon = 0x0080
	iconSmall = 0
	iconBig   = 1

	wmClose = 0x0010
	wmSize  = 0x0005
	wmQuit  = 0x0012
	wmNull  = 0x0000

	sizeMinimized = 1
	swHide        = 0
	swRestore     = 9

	mfString     = 0x0000
	tpmRightBtn  = 0x0002
	tpmReturnCmd = 0x0100

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x1
	nifIcon    = 0x2
	nifTip     = 0x4
	nifInfo    = 0x10
	niffInfo   = 0x1

	wmTrayMsg = 0x8001 // WM_APP+1
	wmAppShow = 0x8002
	wmAppQuit = 0x8003

	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
)

// GWL_WNDPROC = -4（需按 uintptr 表示）
var gwlWndProc = ^uintptr(3)

var (
	shell32 = syscall.NewLazyDLL("shell32.dll")

	procRegisterClassExW   = user32.NewProc("RegisterClassExW")
	procCreateWindowExW    = user32.NewProc("CreateWindowExW")
	procDefWindowProcW     = user32.NewProc("DefWindowProcW")
	procGetMessageW        = user32.NewProc("GetMessageW")
	procTranslateMessage   = user32.NewProc("TranslateMessage")
	procDispatchMessageW   = user32.NewProc("DispatchMessageW")
	procPostMessageW       = user32.NewProc("PostMessageW")
	procShowWindow         = user32.NewProc("ShowWindow")
	procSetForegroundW     = user32.NewProc("SetForegroundWindow")
	procSendMessageW       = user32.NewProc("SendMessageW")
	procSetWindowLongPtrW  = user32.NewProc("SetWindowLongPtrW")
	procCallWindowProcW    = user32.NewProc("CallWindowProcW")
	procCreatePopupMenu    = user32.NewProc("CreatePopupMenu")
	procAppendMenuW        = user32.NewProc("AppendMenuW")
	procTrackPopupMenu     = user32.NewProc("TrackPopupMenu")
	procDestroyMenu        = user32.NewProc("DestroyMenu")
	procGetCursorPos       = user32.NewProc("GetCursorPos")
	procGetModuleHandleW   = kernel32.NewProc("GetModuleHandleW")
	procExtractIconExW     = shell32.NewProc("ExtractIconExW")
	procShellNotifyIconW   = shell32.NewProc("Shell_NotifyIconW")
)

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type msgW struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	PtX     int32
	PtY     int32
	Private uint32
}

type pointW struct {
	X int32
	Y int32
}

// notifyIconData 对齐 Windows x64 的 NOTIFYICONDATAW（984 字节）
type notifyIconData struct {
	CbSize          uint32
	Pad0            uint32
	HWnd            uintptr
	UID             uint32
	Flags           uint32
	CallbackMessage uint32
	Pad1            uint32
	HIcon           uintptr
	SzTip           [128]uint16
	DwState         uint32
	DwStateMask     uint32
	SzInfo          [256]uint16
	UVersion        uint32
	Pad2            uint32
	SzInfoTitle     [64]uint16
	DwInfoFlags     uint32
	Pad3            uint32
	GUID            [16]byte
	HBalloonIcon    uintptr
}

var (
	mainWindow  uintptr
	mainOrigPrc uintptr
	leaving     atomic.Bool
	trayAlready atomic.Bool

	trayMu   sync.Mutex
	trayHwnd uintptr
	trayNid  notifyIconData
	trayInit bool

	mainProcCb = syscall.NewCallback(mainWndProc)
	trayProcCb = syscall.NewCallback(trayWndProc)
)

// Quit 请求正常退出（等价于托盘菜单“退出”：保存任务后销毁窗口）
func Quit() {
	if mainWindow != 0 {
		procPostMessageW.Call(mainWindow, wmAppQuit, 0, 0)
		return
	}
	os.Exit(0)
}

// installWindowIcon 用 exe 内嵌图标设置标题栏与任务栏图标
func installWindowIcon(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	p, err := syscall.UTF16PtrFromString(exe)
	if err != nil {
		return
	}
	var big, small uintptr
	n, _, _ := procExtractIconExW.Call(uintptr(unsafe.Pointer(p)), 0,
		uintptr(unsafe.Pointer(&big)), uintptr(unsafe.Pointer(&small)), 1)
	if n == 0 {
		return
	}
	if big != 0 {
		procSendMessageW.Call(hwnd, wmSetIcon, iconBig, big)
	}
	if small != 0 {
		procSendMessageW.Call(hwnd, wmSetIcon, iconSmall, small)
	}
}

// startTray 启动托盘（隐藏消息窗口 + 独立消息循环），并接管主窗口关闭/最小化
func startTray(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	mainWindow = hwnd
	if p, _, _ := procSetWindowLongPtrW.Call(hwnd, gwlWndProc, mainProcCb); p != 0 {
		mainOrigPrc = p
	}
	go runTray()
}

// stopTray 移除托盘图标并结束托盘消息循环
func stopTray() {
	trayMu.Lock()
	if trayInit {
		procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&trayNid)))
		trayInit = false
	}
	h := trayHwnd
	trayMu.Unlock()
	if h != 0 {
		procPostMessageW.Call(h, wmQuit, 0, 0)
	}
}

func runTray() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hInst, _, _ := procGetModuleHandleW.Call(0)
	clsName, _ := syscall.UTF16PtrFromString("XlauchTrayWnd")
	wc := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   trayProcCb,
		HInstance:     hInst,
		LpszClassName: clsName,
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	title, _ := syscall.UTF16PtrFromString("XlauchTray")
	h, _, _ := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(clsName)),
		uintptr(unsafe.Pointer(title)), 0, 0, 0, 0, 0, 0, 0, hInst, 0)
	if h == 0 {
		return
	}

	// 托盘图标同样取自 exe 内嵌资源
	var big, small uintptr
	if exe, err := os.Executable(); err == nil {
		if p, err := syscall.UTF16PtrFromString(exe); err == nil {
			procExtractIconExW.Call(uintptr(unsafe.Pointer(p)), 0,
				uintptr(unsafe.Pointer(&big)), uintptr(unsafe.Pointer(&small)), 1)
		}
	}
	icon := small
	if icon == 0 {
		icon = big
	}

	trayMu.Lock()
	trayHwnd = h
	trayNid = notifyIconData{
		CbSize:          uint32(unsafe.Sizeof(notifyIconData{})),
		HWnd:            h,
		UID:             1,
		Flags:           nifMessage | nifIcon | nifTip,
		CallbackMessage: wmTrayMsg,
		HIcon:           icon,
	}
	putUTF16(trayNid.SzTip[:], "Xlauch 下载器")
	trayInit = true
	trayMu.Unlock()

	if r, _, _ := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&trayNid))); r == 0 {
		// 添加失败（如资源管理器未就绪）时重试一次
		procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&trayNid)))
	}

	var m msgW
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 || r == ^uintptr(0) {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func trayWndProc(hwnd, msg, wp, lp uintptr) uintptr {
	if msg == wmTrayMsg {
		switch lp & 0xFFFF {
		case wmLButtonUp, wmLButtonDblClk:
			procPostMessageW.Call(mainWindow, wmAppShow, 0, 0)
		case wmRButtonUp:
			showTrayMenu(hwnd)
		}
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

func showTrayMenu(hwnd uintptr) {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)
	showTxt, _ := syscall.UTF16PtrFromString("打开主界面")
	quitTxt, _ := syscall.UTF16PtrFromString("退出")
	procAppendMenuW.Call(menu, mfString, 1, uintptr(unsafe.Pointer(showTxt)))
	procAppendMenuW.Call(menu, mfString, 2, uintptr(unsafe.Pointer(quitTxt)))

	var pt pointW
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundW.Call(hwnd) // 不加这步右键菜单会点外不消失
	r, _, _ := procTrackPopupMenu.Call(menu, tpmRightBtn|tpmReturnCmd,
		uintptr(pt.X), uintptr(pt.Y), 0, hwnd, 0)
	procPostMessageW.Call(hwnd, wmNull, 0, 0)
	switch r {
	case 1:
		procPostMessageW.Call(mainWindow, wmAppShow, 0, 0)
	case 2:
		procPostMessageW.Call(mainWindow, wmAppQuit, 0, 0)
	}
}

// mainWndProc 接管主窗口：关闭/最小化改为隐藏到托盘，退出时才真正销毁
func mainWndProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch msg {
	case wmClose:
		if !leaving.Load() && closeToTray.Load() {
			hideToTray(hwnd)
			return 0
		}
	case wmSize:
		if wp == sizeMinimized && !leaving.Load() {
			hideToTray(hwnd)
			return 0
		}
	case wmAppShow:
		procShowWindow.Call(hwnd, swRestore)
		procSetForegroundW.Call(hwnd)
		return 0
	case wmAppQuit:
		leaving.Store(true)
		return callMainProc(hwnd, wmClose, 0, 0)
	}
	return callMainProc(hwnd, msg, wp, lp)
}

func callMainProc(hwnd, msg, wp, lp uintptr) uintptr {
	if mainOrigPrc != 0 {
		r, _, _ := procCallWindowProcW.Call(mainOrigPrc, hwnd, msg, wp, lp)
		return r
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

func hideToTray(hwnd uintptr) {
	procShowWindow.Call(hwnd, swHide)
	if trayAlready.CompareAndSwap(false, true) {
		trayMu.Lock()
		if trayInit {
			n := trayNid
			n.Flags |= nifInfo
			putUTF16(n.SzInfoTitle[:], "Xlauch 下载器")
			putUTF16(n.SzInfo[:], "已最小化到托盘，左键点击托盘图标可恢复窗口")
			n.DwInfoFlags = niffInfo
			procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&n)))
		}
		trayMu.Unlock()
	}
}

func putUTF16(dst []uint16, s string) {
	u := utf16.Encode([]rune(s))
	if len(u) >= len(dst) {
		u = u[:len(dst)-1]
	}
	copy(dst, u)
}
