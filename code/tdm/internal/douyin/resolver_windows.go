//go:build windows

package douyin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"

	"tdm/internal/config"
	"tdm/internal/ui"
)

var (
	brOnce sync.Once
	brErr  error
	brWV   webview2.WebView
	brMu   sync.Mutex
	caps   = make(chan capMsg, 64)
)

type capMsg struct {
	URL  string
	Body string
}

var (
	procShowWindow    = windows.NewLazySystemDLL("user32.dll").NewProc("ShowWindow")
	procSetWindowPos  = windows.NewLazySystemDLL("user32.dll").NewProc("SetWindowPos")
	procGetWindowLong = windows.NewLazySystemDLL("user32.dll").NewProc("GetWindowLongPtrW")
	procSetWindowLong = windows.NewLazySystemDLL("user32.dll").NewProc("SetWindowLongPtrW")
	procSetLayered    = windows.NewLazySystemDLL("user32.dll").NewProc("SetLayeredWindowAttributes")
	procGetForeground = windows.NewLazySystemDLL("user32.dll").NewProc("GetForegroundWindow")
	procSetForeground = windows.NewLazySystemDLL("user32.dll").NewProc("SetForegroundWindow")
)

const (
	swShowNoActivate = 4    // SW_SHOWNOACTIVATE
	swpNoZOrder      = 0x4  // SWP_NOZORDER
	swpNoActivate    = 0x10 // SWP_NOACTIVATE

	gwlExStyle     = ^uintptr(19) // GWL_EXSTYLE = -20
	wsExToolWindow = 0x00000080   // 不进任务栏 / Alt-Tab
	wsExLayered    = 0x00080000   // 分层窗口，支持整体透明
	wsExNoActivate = 0x08000000   // 鼠标点击不抢前台
	lwaAlpha       = 0x2          // LWA_ALPHA
)

// dbg 仅在设置 TDM_DOUYIN_DEBUG 时输出诊断日志，便于排查抖音验证/超时问题。
func dbg(format string, a ...any) {
	if os.Getenv("TDM_DOUYIN_DEBUG") == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(config.DataDir(), "douyin-resolver.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
}

// makeBackground 把解析窗口完全后台化，用户不可感知：
//   - 工具窗口样式：不出现在任务栏与 Alt-Tab；
//   - 分层窗口 + alpha=0：像素级完全透明。窗口对系统仍保持"可见"状态
//     （IsWindowVisible=true、未最小化），Chromium 不会对页面降频限流；
//   - 移动到 (-20000,-20000)：远离任何现实中的多屏组合，兜底防露出；
//   - 归还前台焦点：WebView2 库创建窗口时会短暂抢占用户焦点。
func makeBackground(hwnd, prevForeground uintptr) {
	off := int64(-20000) // 远离任何现实中的多屏组合（每边可容纳 5 块 4K 屏）
	r0, _, _ := procGetWindowLong.Call(hwnd, gwlExStyle)
	procSetWindowLong.Call(hwnd, gwlExStyle, r0|wsExToolWindow|wsExNoActivate|wsExLayered)
	_, _, _ = procSetLayered.Call(hwnd, 0, 0, lwaAlpha)
	_, _, _ = procSetWindowPos.Call(hwnd, 0, uintptr(off), uintptr(off),
		1024, 720, swpNoZOrder|swpNoActivate)
	_, _, _ = procShowWindow.Call(hwnd, swShowNoActivate)
	if prevForeground != 0 && prevForeground != hwnd {
		_, _, _ = procSetForeground.Call(prevForeground)
	}
}

// ensureBrowser 启动一个完全后台化的 WebView2（独立线程与消息循环）。
// 不能用 SW_HIDE 真正隐藏窗口：Chromium 会把隐藏页面降频，抖音页面因此不再请求详情接口；
// 因此采用"保持可见 + 分层透明 alpha=0 + 移出屏幕 + 工具窗口样式"的组合实现无感后台。
func ensureBrowser() error {
	brOnce.Do(func() {
		ready := make(chan error, 1)
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			if err := ui.EnsureRuntime(); err != nil {
				ready <- err
				return
			}
			fg, _, _ := procGetForeground.Call() // 创建窗口前记录前台窗口
			w := webview2.NewWithOptions(webview2.WebViewOptions{
				Debug:    false,
				DataPath: filepath.Join(config.DataDir(), "webview-douyin"),
				WindowOptions: webview2.WindowOptions{
					Title:  "douyin-resolver",
					Width:  1024,
					Height: 720,
				},
			})
			if w == nil {
				ready <- fmt.Errorf("WebView2 初始化失败")
				return
			}
			makeBackground(uintptr(w.Window()), fg)
			_ = w.Bind("dyCapture", func(s string) {
				var ev struct {
					URL  string `json:"url"`
					Body string `json:"body"`
				}
				if json.Unmarshal([]byte(s), &ev) == nil {
					select {
					case caps <- capMsg{URL: ev.URL, Body: ev.Body}:
					default:
					}
				}
			})
			w.Init(hookScript)
			brWV = w
			brErr = nil
			ready <- nil
			w.Run()
		}()
		brErr = <-ready
	})
	return brErr
}

// fetchDetail 用内置浏览器打开视频页，截取页面自身的 aweme/detail 请求结果。
func fetchDetail(ctx context.Context, raw string) (*detailData, error) {
	id, err := AwemeID(ctx, raw)
	if err != nil {
		return nil, err
	}
	if err := ensureBrowser(); err != nil {
		return nil, err
	}

	brMu.Lock()
	defer func() {
		navigate("about:blank")
		time.Sleep(250 * time.Millisecond)
		brMu.Unlock()
	}()

	for {
		select {
		case <-caps:
		default:
			goto clearDone
		}
	}

clearDone:
	dbg("start id=%s", id)

	// 注入浏览器扩展导入的登录 Cookie（含 httpOnly），降低人机验证触发率
	injectDouyinCookies()

	// 先访问首页建立 ttwid 等 Cookie，直接深链更容易触发强制验证。
	navigate("https://www.douyin.com/")
	if !sleepCtx(ctx, 4*time.Second) {
		return nil, ctx.Err()
	}
	dbg("open video page")
	navigate("https://www.douyin.com/video/" + id)

	deadline := time.NewTimer(75 * time.Second)
	defer deadline.Stop()
	count := 0
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			dbg("timeout, captured=%d", count)
			return nil, fmt.Errorf("抖音页面加载超时（可能触发了人机验证），请稍后重试")
		case ev := <-caps:
			count++
			dbg("capture[%d] url=%s len=%d", count, ev.URL, len(ev.Body))
			if strings.HasPrefix(ev.URL, "diag:") {
				body := ev.Body
				if len([]rune(body)) > 160 {
					body = string([]rune(body)[:160])
				}
				dbg("page=%s text=%s", strings.TrimPrefix(ev.URL, "diag:"), body)
				continue
			}
			if strings.HasPrefix(ev.URL, "router:") {
				if d, rerr := parseRouterData([]byte(ev.Body), id); rerr == nil {
					dbg("router parsed id=%s", d.AwemeID)
					return d, nil
				} else if rerr != nil {
					head := ev.Body
					if len(head) > 600 {
						head = head[:600]
					}
					dbg("router parse failed: %v head=%s", rerr, head)
				}
				continue
			}
			if !strings.Contains(ev.URL, "aweme_id="+id) || !strings.Contains(ev.Body, `"aweme_detail"`) {
				continue
			}
			return parseDetail([]byte(ev.Body))
		}
	}
}

// navigate 通过消息循环在浏览器线程执行，WebView2 的 COM 调用不能跨线程直接发起。
func navigate(url string) {
	if brWV == nil {
		return
	}
	brWV.Dispatch(func() { brWV.Navigate(url) })
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// hookScript 在页面脚本执行前注入：捕获详情接口响应并静音播放器。
const hookScript = `(function(){
  function send(url, body){
    try { window.dyCapture(JSON.stringify({url:String(url), body:String(body)})); } catch(e){}
  }
  var of = window.fetch;
  if (of) {
    window.fetch = function(input, init){
      var url = (typeof input === 'string') ? input : ((input && input.url) || '');
      return of.apply(this, arguments).then(function(res){
        try {
          if (String(url).indexOf('/aweme/detail') > -1 && res.clone) {
            res.clone().text().then(function(t){ send(url, t); });
          }
        } catch(e){}
        return res;
      });
    };
  }
  var XO = XMLHttpRequest.prototype.open;
  XMLHttpRequest.prototype.open = function(m, u){
    var url = String(u);
    if (url.indexOf('/aweme/detail') > -1) {
      this.addEventListener('load', function(){ send(url, this.responseText); });
    }
    return XO.apply(this, arguments);
  };
  setInterval(function(){
    try {
      document.querySelectorAll('video').forEach(function(v){
        v.muted = true;
        if (!v.paused) { v.pause(); }
      });
    } catch(e){}
  }, 1200);
  setTimeout(function(){
    try {
      var text = document.body ? (document.body.innerText || '') : '';
      send('diag:' + location.href, (document.title || '') + ' | ' + text.slice(0, 300));
    } catch(e){}
  }, 3000);
  var sentRouter = false;
  function sendRouter(){
    if (sentRouter) { return; }
    try {
      if (window._ROUTER_DATA) {
        sentRouter = true;
        send('router:' + location.href, JSON.stringify(window._ROUTER_DATA));
      }
    } catch(e){}
  }
  setTimeout(sendRouter, 2500);
  setInterval(sendRouter, 1500);
})()`
