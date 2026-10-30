//go:build windows

package main

import (
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

func wndProcThrottle(hwnd uintptr, msg uint32, _ uintptr, _ uintptr) (uintptr, bool) {
	switch msg {
	case w32.WM_ENTERSIZEMOVE:
		inSizeMove.Store(true)
		return 0, false
	case w32.WM_EXITSIZEMOVE:
		inSizeMove.Store(false)
		// Let this one through, then queue a WM_MOVE for the final sync.
		w32.PostMessage(w32.HWND(hwnd), w32.WM_MOVE, 0, 0)
		return 0, false
	case w32.WM_MOVE, w32.WM_MOVING:
		if inSizeMove.Load() {
			return 0, true // swallow: skip Wails' sync COM notify
		}
		return 0, false
	}
	return 0, false
}
