//go:build windows

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/wailsapp/wails/v3/pkg/w32"
)

// Wails calls the synchronous WebView2 COM method
// NotifyParentWindowPositionChanged on every WM_MOVE/WM_MOVING — hundreds
// of cross-process round-trips per drag stall the UI thread and the window
// lags behind the cursor. This interceptor swallows ALL of them during a
// drag and posts a single WM_MOVE on drag end so the controller re-syncs
// to the resting position (popup anchoring needs it exactly once).
// Outside a size/move session (programmatic moves, snap) messages pass
// through untouched.
// 拖动期全吞：Wails 对每条 WM_MOVE/WM_MOVING 都同步 COM 通知控制器，
// 一次拖拽几百次跨进程往返卡死 UI 线程 → 窗口不跟手。
// 拦截器在 ENTERSIZEMOVE..EXITSIZEMOVE 之间吞掉全部 move 通知，
// 松手补发一条 WM_MOVE 同步最终落点；非拖动脉冲（程序化移动/吸附）
// 原样放行。
var inSizeMove atomic.Bool

func init() {
	// Equivalent of the manifest's PerMonitorV2 — the dev-built exe has no
	// embedded manifest (rsrc breaks SxS), so set awareness programmatically
	// before any HWND exists. 等价 manifest 的 PerMonitorV2：开发构建无
	// 内嵌 manifest（rsrc 会损坏 SxS），改在进程启动时程序化声明。
	_ = w32.SetProcessDpiAwarenessContext(w32.DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2)
}

func wndProcThrottle(hwnd uintptr, msg uint32, _ uintptr, _ uintptr) (uintptr, bool) {
	switch msg {
	case w32.WM_ENTERSIZEMOVE:
		inSizeMove.Store(true)
		return 0, false
	case w32.WM_EXITSIZEMOVE:
		inSizeMove.Store(false)
		// Let this one through, then queue a WM_MOVE for the final sync.
		w32.PostMessage(w32.HWND(hwnd), w32.WM_MOVE, 0, 0)
		saveWindowRect(hwnd)
		return 0, false
	case w32.WM_CLOSE:
		saveWindowRect(hwnd)
		return 0, false
	case w32.WM_MOVE, w32.WM_MOVING:
		if inSizeMove.Load() {
			return 0, true // swallow: skip Wails' sync COM notify
		}
		return 0, false
	}
	return 0, false
}

// ---- window placement persistence / 窗口位置记忆 ----
// Saved on drag/resize end (WM_EXITSIZEMOVE) and close (WM_CLOSE) to
// %APPDATA%\wittychat.exe\window.json; restored at next launch when the
// rect still intersects the virtual screen (handles unplugged monitors).
// 拖动/调整结束与关闭时持久化到用户配置目录，下次启动若位置仍在虚拟
// 屏幕内则恢复（防拔掉显示器后窗口开到屏幕外）。

type winRect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func windowStatePath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "wittychat.exe", "window.json")
}

func saveWindowRect(hwnd uintptr) {
	rc := w32.GetWindowRect(w32.HWND(hwnd))
	if rc == nil {
		return
	}
	r := winRect{X: int(rc.Left), Y: int(rc.Top), W: int(rc.Right - rc.Left), H: int(rc.Bottom - rc.Top)}
	if r.W < 200 || r.H < 100 {
		return
	}
	p := windowStatePath()
	if p == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if b, err := json.Marshal(r); err == nil {
		_ = os.WriteFile(p, b, 0o600)
	}
}

// savedWindowRect returns the persisted rect if it still overlaps the
// virtual screen (all monitors) by at least 60px.
func savedWindowRect() (winRect, bool) {
	var r winRect
	p := windowStatePath()
	if p == "" {
		return r, false
	}
	b, err := os.ReadFile(p)
	if err != nil || json.Unmarshal(b, &r) != nil || r.W < 200 || r.H < 100 {
		return r, false
	}
	vx := w32.GetSystemMetrics(76) // SM_XVIRTUALSCREEN
	vy := w32.GetSystemMetrics(77) // SM_YVIRTUALSCREEN
	vw := w32.GetSystemMetrics(78) // SM_CXVIRTUALSCREEN
	vh := w32.GetSystemMetrics(79) // SM_CYVIRTUALSCREEN
	const m = 60                   // min visible overlap / 最小可见重叠
	if r.X > vx+vw-m || r.X+r.W < vx+m || r.Y > vy+vh-m || r.Y+r.H < vy+m {
		return r, false
	}
	return r, true
}
