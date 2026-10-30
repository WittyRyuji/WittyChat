//go:build windows

package main

import (
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/w32"
)

// Wails calls the synchronous WebView2 COM method
// NotifyParentWindowPositionChanged on every WM_MOVE/WM_MOVING — hundreds
// of cross-process round-trips per drag stall the UI thread and the window
// lags behind the cursor. This interceptor lets at most one notification
// through per 50 ms and posts a final WM_MOVE on drag end so the
// controller learns the resting position.
// 拖动限流：Wails 对每条 WM_MOVE/WM_MOVING 都同步 COM 通知 WebView2
// 控制器，一次拖拽几百次跨进程往返卡死 UI 线程 → 窗口不跟手。
// 此拦截器每 50ms 放行一次，拖动结束时补发 WM_MOVE 同步最终位置
// （弹层锚点需要控制器知道窗口落点）。
var lastMoveNotify atomic.Int64

func wndProcThrottle(hwnd uintptr, msg uint32, _ uintptr, _ uintptr) (uintptr, bool) {
	switch msg {
	case w32.WM_EXITSIZEMOVE:
		// Drag ended: let the posted WM_MOVE through (gate reset) so the
		// controller re-syncs to the final window position exactly once.
		lastMoveNotify.Store(0)
		w32.PostMessage(w32.HWND(hwnd), w32.WM_MOVE, 0, 0)
		return 0, false
	case w32.WM_MOVE, w32.WM_MOVING:
		now := time.Now().UnixMilli()
		for {
			last := lastMoveNotify.Load()
			if now-last < 50 {
				return 0, true // swallow: skip Wails' sync COM notify
			}
			if lastMoveNotify.CompareAndSwap(last, now) {
				return 0, false
			}
		}
	}
	return 0, false
}
