//go:build !windows

package main

// No-op on non-Windows platforms — the interceptor is only wired into
// WindowsOptions anyway.
func wndProcThrottle(uintptr, uint32, uintptr, uintptr) (uintptr, bool) {
	return 0, false
}

// Window placement persistence is Windows-only for now.
type winRect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func savedWindowRect() (winRect, bool) { return winRect{}, false }
