//go:build windows

package tray

// win32 is the thin interface over the Win32 calls needed by the tray.
// The real implementation (realWin32) calls into shell32/user32 via x/sys/windows.
// Tests inject a fakeWin32 that records calls without touching the desktop.
//
// All methods are expected to be called ONLY from the message-pump goroutine
// (the one with runtime.LockOSThread). No locking is provided here.
type win32 interface {
	// RegisterTrayIcon adds the notification icon to the system tray.
	// Returns the HWND of the hidden message window and an error.
	RegisterTrayIcon(callbackMsg uint32) (hwnd uintptr, err error)

	// UpdateTrayIcon refreshes the icon tooltip. Used after a status change.
	UpdateTrayIcon(hwnd uintptr, tooltip string) error

	// ShowBalloon shows a balloon/toast notification anchored to the tray icon.
	// Used by the updater to surface "update available" / "restart to apply".
	ShowBalloon(hwnd uintptr, title, message string) error

	// RemoveTrayIcon removes the icon from the tray on cleanup.
	RemoveTrayIcon(hwnd uintptr) error

	// ShowContextMenu displays the popup menu at the current cursor position.
	// Returns the selected MenuItemID, or 0 if the user dismissed the menu.
	ShowContextMenu(hwnd uintptr, items []MenuItem) (MenuItemID, error)

	// PumpMessages runs the Win32 message loop. It blocks until WM_QUIT is
	// posted to the thread (via PostQuitMessage) or quit is closed.
	// It calls onCallback when the tray callback message arrives.
	PumpMessages(hwnd uintptr, quit <-chan struct{}, onCallback func(msg uint32, wparam, lparam uintptr))
}
