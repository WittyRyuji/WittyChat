//go:build !windows

package main

// No-op on non-Windows platforms — the interceptor is only wired into
// WindowsOptions anyway.
func wndProcThrottle(uintptr, uint32, uintptr, uintptr) (uintptr, bool) {
	return 0, false
}
