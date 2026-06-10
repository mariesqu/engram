//go:build windows

package tray

// RunMessagePump is exposed for use by cmd/engram/tray.go and tests.
// It is the document of record for the threading contract:
//   - MUST be called in a goroutine that has already called runtime.LockOSThread().
//   - Calls GetMessage/TranslateMessage/DispatchMessage in a loop.
//   - Exits on WM_QUIT (r == 0) or a GetMessage error (r == -1, i.e. ^uintptr(0)).
//   - Exits when quit is closed (a monitoring goroutine posts WM_QUIT).
//
// This function is intentionally thin — the real pump loop lives in
// realWin32.PumpMessages, which is called by runTray via the win32 interface.
// RunMessagePump is provided as a named export so tests can reference the
// documented contract without calling into the Win32 layer.
//
// Thread safety: the caller MUST hold the OS thread lock. Do not call from
// multiple goroutines simultaneously for the same HWND.
func RunMessagePump(hwnd uintptr, quit <-chan struct{}) {
	w := &realWin32{}
	w.PumpMessages(hwnd, quit, func(_ uint32, _, _ uintptr) {})
}
