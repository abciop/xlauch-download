//go:build windows

package douyin

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/jchv/go-webview2"
	"github.com/jchv/go-webview2/pkg/edge"
	"golang.org/x/sys/windows"

	"tdm/internal/ytdlp"
)

// 抖音解析浏览器 Cookie 注入。
//
// 库 go-webview2 只封装到 ICoreWebView2_2/_3 的 vtable 声明，没有实现
// ICoreWebView2CookieManager。这里按官方 WebView2.h 的 vtable 顺序自行补齐。
//
// 关键教训（槽位错 = 野指针 = 进程直接崩溃）：
//  1. GetCookieManager 在 ICoreWebView2_2 的 vtable 槽 66（IUnknown(3) +
//     ICoreWebView2(58) + _2 前 5 个方法之后）。基接口 ICoreWebView2 的 vtable
//     只有 61 槽，绝不能在基接口指针上读槽 66——越界读到野指针，COM 调用
//     会当场崩掉整个进程。必须先 QueryInterface 到 _3（库自带
//     GetICoreWebView2_3，_3 的 vtable 内嵌 _2，槽 66 同样是 GetCookieManager）。
//  2. ICoreWebView2Cookie 真实槽位（get_Name/get_Value/put_Value 占据 3-5）：
//     get_Name(3) get_Value(4) put_Value(5) get_Domain(6) get_Path(7)
//     get_Expires(8) put_Expires(9) get_IsHttpOnly(10) put_IsHttpOnly(11)
//     get_SameSite(12) put_SameSite(13) get_IsSecure(14) put_IsSecure(15)
//     get_IsSession(16)。put_IsHttpOnly 不是 8（8 是 get_Expires，会把 0/1
//     当指针写入，同样直接崩溃）。
//  3. ICoreWebView2CookieManager：CreateCookie(3) CopyCookie(4) GetCookies(5)
//     AddOrUpdateCookie(6) DeleteCookie(7) DeleteCookies(8)
//     DeleteCookiesWithDomainAndPath(9) DeleteAllCookies(10)。
//
// 说明：put_Expires 参数为 double，Windows x64 下经 XMM 寄存器传递，Go 的
// syscall 无法设置 XMM 寄存器，故不设置过期时间（会话 Cookie，默认 -1.0）。
// 浏览器进程存活期间 Cookie 一直有效，且每次解析前都会按文件版本重新注入。
const (
	vtRelease           = 2  // IUnknown::Release（所有 COM 对象均为槽 2）
	vtGetCookieManager  = 66 // 见上方教训 1
	vtMgrCreateCookie   = 3
	vtMgrAddOrUpdate    = 6
	vtCookiePutHttpOnly = 11
	vtCookiePutSecure   = 15
)

// comCall 按槽位调用 COM 对象的 vtable 方法（this + args）。
func comCall(obj uintptr, slot int, args ...uintptr) (uintptr, error) {
	vtbl := *(*uintptr)(unsafe.Pointer(obj))
	proc := *(*uintptr)(unsafe.Pointer(vtbl + uintptr(slot)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := edge.ComProc(proc).Call(append([]uintptr{obj}, args...)...)
	if int64(r) < 0 {
		return r, fmt.Errorf("HRESULT %08X", uint32(r))
	}
	return r, nil
}

// coreWebView 从 webview2.WebView 反射提取底层 *edge.ICoreWebView2（库未导出该字段）。
func coreWebView(w webview2.WebView) *edge.ICoreWebView2 {
	rv := reflect.ValueOf(w)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return nil
	}
	b := rv.Elem().FieldByName("browser")
	if !b.IsValid() {
		return nil
	}
	iface := reflect.NewAt(b.Type(), unsafe.Pointer(b.UnsafeAddr())).Elem().Interface()
	cv := reflect.ValueOf(iface)
	if cv.Kind() != reflect.Ptr || cv.IsNil() {
		return nil
	}
	f := cv.Elem().FieldByName("webview")
	if !f.IsValid() || f.IsNil() {
		return nil
	}
	core := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface()
	c, _ := core.(*edge.ICoreWebView2)
	return c
}

var injectedMod atomic.Int64

// injectDouyinCookies 把扩展导入的 douyin.com Cookie 注入内置解析浏览器。
// httpOnly Cookie 无法用 document.cookie 设置，必须走 CookieManager。
func injectDouyinCookies() {
	cookies := ytdlp.LoadSiteCookies("douyin.com")
	if len(cookies) == 0 || brWV == nil {
		return
	}
	fi, err := os.Stat(filepath.Join(ytdlp.CookieDir(), "douyin.com.json"))
	if err != nil {
		return
	}
	if fi.ModTime().Unix() == injectedMod.Load() {
		return // 该版本已注入过
	}
	done := make(chan struct{})
	brWV.Dispatch(func() {
		defer close(done)
		core := coreWebView(brWV)
		if core == nil {
			dbg("cookie: no core webview")
			return
		}
		// GetCookieManager 只存在于 ICoreWebView2_2 及更高版本的 vtable。
		// 库自带的 GetICoreWebView2_3 会做 QueryInterface（IID 来自库源码），
		// _3 的 vtable 内嵌 _2 部分，槽 66 同样是 GetCookieManager。
		wv3 := core.GetICoreWebView2_3()
		if wv3 == nil {
			dbg("cookie: QueryInterface ICoreWebView2_3 failed")
			return
		}
		if err := injectCOM(uintptr(unsafe.Pointer(wv3)), cookies); err != nil {
			dbg("cookie inject failed: %v", err)
			return
		}
		injectedMod.Store(fi.ModTime().Unix())
		dbg("cookie inject ok: %d cookies", len(cookies))
	})
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		dbg("cookie inject: dispatch timeout")
	}
}

// injectCOM 在浏览器 UI 线程执行注入：GetCookieManager → CreateCookie → AddOrUpdateCookie。
// wv3 必须是 QueryInterface 得到的 ICoreWebView2_3 接口指针（vtable 内嵌 _2 部分，
// 槽 66 = GetCookieManager）；用基接口指针读高槽位会越界读到野指针，直接崩进程。
func injectCOM(wv3 uintptr, cookies []ytdlp.CookieEntry) error {
	// QueryInterface 增加的引用计数用完释放
	defer func() { _, _ = comCall(wv3, vtRelease) }()

	var mgr uintptr
	if _, err := comCall(wv3, vtGetCookieManager, uintptr(unsafe.Pointer(&mgr))); err != nil {
		return fmt.Errorf("GetCookieManager: %w", err)
	}
	if mgr == 0 {
		return fmt.Errorf("CookieManager 为空")
	}
	defer func() { _, _ = comCall(mgr, vtRelease) }()
	ok := 0
	for _, c := range cookies {
		name, err1 := windows.UTF16PtrFromString(c.Name)
		val, err2 := windows.UTF16PtrFromString(c.Value)
		dom, err3 := windows.UTF16PtrFromString(c.Domain)
		pth, err4 := windows.UTF16PtrFromString(c.Path)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			continue
		}
		var ck uintptr
		if _, err := comCall(mgr, vtMgrCreateCookie,
			uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(val)),
			uintptr(unsafe.Pointer(dom)), uintptr(unsafe.Pointer(pth)),
			uintptr(unsafe.Pointer(&ck))); err != nil {
			return fmt.Errorf("CreateCookie(%s): %w", c.Name, err)
		}
		if ck == 0 {
			continue
		}
		_, _ = comCall(ck, vtCookiePutHttpOnly, b2i(c.HTTPOnly))
		_, _ = comCall(ck, vtCookiePutSecure, b2i(c.Secure))
		_, _ = comCall(mgr, vtMgrAddOrUpdate, ck)
		_, _ = comCall(ck, vtRelease)
		ok++
	}
	if ok == 0 {
		return fmt.Errorf("没有可注入的 Cookie")
	}
	return nil
}

func b2i(b bool) uintptr {
	if b {
		return 1
	}
	return 0
}
