//go:build windows

// Package tray implements a Windows system-tray icon for the engram resident
// daemon using Shell_NotifyIcon via golang.org/x/sys/windows syscalls.
//
// Threading model
// ───────────────
// Win32 window and message APIs are thread-affine: all calls that create a
// window or post messages to it MUST happen on the same OS thread for the
// lifetime of the window.  Go's runtime scheduler can move goroutines between
// OS threads, so the pump goroutine calls runtime.LockOSThread() to pin itself
// to one OS thread permanently.
//
// The pump goroutine NEVER performs HTTP calls.  When a menu item is selected,
// it sends the ActionFunc to a buffered channel.  A separate worker goroutine
// reads the channel and executes the action — HTTP calls, browser launches, etc.
// happen there.  This guarantees the message loop stays responsive.
//
// Win32 surface used
// ──────────────────
//
//	shell32.dll  Shell_NotifyIconW  (add/modify/delete the tray icon)
//	user32.dll   RegisterClassExW   (register the hidden message-window class)
//	             CreateWindowExW    (create the hidden message window)
//	             DefWindowProcW     (default message dispatch)
//	             DestroyWindow      (clean up the message window)
//	             PostQuitMessage    (signal the message loop to exit)
//	             GetMessage         (block-read next message)
//	             TranslateMessage   (translate key messages)
//	             DispatchMessage    (dispatch to WndProc)
//	             GetCursorPos       (get mouse position for TrackPopupMenu)
//	             SetForegroundWindow (required before TrackPopupMenu)
//	             CreatePopupMenu    (create the context menu)
//	             AppendMenuW        (add items to the menu)
//	             TrackPopupMenu     (show the menu at the cursor)
//	             DestroyMenu        (free menu handle)
//	kernel32.dll LoadLibraryW / GetProcAddress (proc loading via windows.MustLoadDLL)
package tray

import (
	"fmt"
	"log"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ── Win32 constants ───────────────────────────────────────────────────────────

const (
	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004

	wm_User         = 0x0400
	wm_TrayCallback = wm_User + 1 // private: tray callback message

	mf_String    = 0x00000000
	mf_Disabled  = 0x00000002
	mf_Grayed    = 0x00000001
	mf_Separator = 0x00000800

	tpm_LeftButton = 0x0000
	tpm_ReturnCmd  = 0x0100
	tpm_LeftAlign  = 0x0000
	tpm_NoNotify   = 0x0080

	cs_HRedraw = 0x0002
	cs_VRedraw = 0x0001

	ws_Overlapped = 0x00000000

	hwnd_Message = ^uintptr(2) // HWND_MESSAGE = ((HWND)-3)
)

// ── NOTIFYICONDATAW struct ────────────────────────────────────────────────────
// We define only the fields we use. The struct must be 952 bytes for Vista+.
type notifyIconDataW struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16 // 128 UTF-16 chars
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     uintptr
}

// ── WNDCLASSEXW struct ────────────────────────────────────────────────────────
type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

// ── MSG struct ────────────────────────────────────────────────────────────────
type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type point struct{ x, y int32 }

// ── realWin32 — live Win32 implementation ────────────────────────────────────

// loadedProcs caches the DLL procedures (loaded once at init).
var (
	modShell32 = windows.MustLoadDLL("shell32.dll")
	modUser32  = windows.MustLoadDLL("user32.dll")

	procShellNotifyIcon   = modShell32.MustFindProc("Shell_NotifyIconW")
	procRegisterClassEx   = modUser32.MustFindProc("RegisterClassExW")
	procCreateWindowEx    = modUser32.MustFindProc("CreateWindowExW")
	procDefWindowProc     = modUser32.MustFindProc("DefWindowProcW")
	procDestroyWindow     = modUser32.MustFindProc("DestroyWindow")
	procPostQuitMessage   = modUser32.MustFindProc("PostQuitMessage")
	procGetMessage        = modUser32.MustFindProc("GetMessageW")
	procTranslateMessage  = modUser32.MustFindProc("TranslateMessage")
	procDispatchMessage   = modUser32.MustFindProc("DispatchMessageW")
	procGetCursorPos      = modUser32.MustFindProc("GetCursorPos")
	procSetForegroundWin  = modUser32.MustFindProc("SetForegroundWindow")
	procCreatePopupMenu   = modUser32.MustFindProc("CreatePopupMenu")
	procAppendMenu        = modUser32.MustFindProc("AppendMenuW")
	procTrackPopupMenu    = modUser32.MustFindProc("TrackPopupMenu")
	procDestroyMenu       = modUser32.MustFindProc("DestroyMenu")
	procCreateIconFromRes = modUser32.MustFindProc("CreateIconFromResourceEx")
)

// wndProcMap maps HWND → callback for the global WndProc.
var (
	wndProcMu  sync.RWMutex
	wndProcMap = map[uintptr]func(uint32, uintptr, uintptr){}
)

// globalWndProc is the Win32 WndProc callback. It is registered with
// RegisterClassExW and called by DispatchMessage on the pump goroutine.
// It must have the exact WNDPROC signature.
func globalWndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	wndProcMu.RLock()
	fn := wndProcMap[hwnd]
	wndProcMu.RUnlock()
	if fn != nil {
		fn(msg, wParam, lParam)
	}
	r, _, _ := procDefWindowProc.Call(hwnd, uintptr(msg), wParam, lParam)
	return r
}

type realWin32 struct {
	hIcon uintptr // loaded from iconData via CreateIconFromResourceEx
}

func newRealWin32() (*realWin32, error) {
	// Load the icon from the embedded .ico bytes using CreateIconFromResourceEx.
	// Win32 expects a pointer to the raw DIB data inside the .ico file, which
	// for a monochrome icon starts after the ICONDIRENTRY (6 + 16*n bytes).
	// We use the first image (16×16) at offset 6 + 2*16 = 38.
	const icoHeaderSize = 6
	const icoDirEntrySize = 16
	iconOffset := icoHeaderSize + 2*icoDirEntrySize // 38 bytes (2 images in our ICO)
	if len(iconData) <= iconOffset {
		return nil, fmt.Errorf("tray: embedded icon data too small (%d bytes)", len(iconData))
	}

	imgData := iconData[iconOffset:]
	hIcon, _, err := procCreateIconFromRes.Call(
		uintptr(unsafe.Pointer(&imgData[0])),
		uintptr(len(imgData)),
		1,          // fIcon = TRUE (icon, not cursor)
		0x00030000, // dwVer = 0x00030000 (Win32 3.x compatible)
		16, 16,     // desired size
		0x0000, // LR_DEFAULTCOLOR
	)
	if hIcon == 0 {
		// Non-fatal: fall back to 0 (system default icon).
		log.Printf("tray: CreateIconFromResourceEx failed: %v — using default icon", err)
	}
	return &realWin32{hIcon: hIcon}, nil
}

// className is the Win32 window class name for the hidden message window.
var className = windows.StringToUTF16Ptr("EngramTrayMsgWindow")

func (w *realWin32) RegisterTrayIcon(callbackMsg uint32) (uintptr, error) {
	// GetModuleHandleEx with flags=0 and moduleName=nil returns the handle of
	// the calling module (equivalent to GetModuleHandle(NULL)).
	var hInst windows.Handle
	if err := windows.GetModuleHandleEx(0, nil, &hInst); err != nil {
		return 0, fmt.Errorf("tray: GetModuleHandleEx: %w", err)
	}

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		style:         cs_HRedraw | cs_VRedraw,
		lpfnWndProc:   windows.NewCallback(globalWndProc),
		hInstance:     uintptr(hInst),
		lpszClassName: className,
	}
	if _, _, err := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); err.(windows.Errno) != 0 {
		// ERROR_CLASS_ALREADY_EXISTS (1410) is acceptable if we re-register.
		if err.(windows.Errno) != 1410 {
			return 0, fmt.Errorf("tray: RegisterClassExW: %w", err)
		}
	}

	hwnd, _, err := procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("engram"))),
		ws_Overlapped,
		0, 0, 0, 0,
		hwnd_Message, // HWND_MESSAGE — hidden message-only window
		0,
		uintptr(hInst),
		0,
	)
	if hwnd == 0 {
		return 0, fmt.Errorf("tray: CreateWindowExW: %w", err)
	}

	// Register our per-HWND callback so globalWndProc can dispatch.
	wndProcMu.Lock()
	wndProcMap[hwnd] = func(m uint32, wParam, lParam uintptr) {
		// The WndProc body is handled in PumpMessages via the onCallback closure.
		_ = m
		_ = wParam
		_ = lParam
	}
	wndProcMu.Unlock()

	tip, _ := windows.UTF16FromString("engram")
	var nid notifyIconDataW
	nid.cbSize = uint32(unsafe.Sizeof(nid))
	nid.hWnd = hwnd
	nid.uID = 1
	nid.uFlags = nifMessage | nifIcon | nifTip
	nid.uCallbackMessage = callbackMsg
	nid.hIcon = w.hIcon
	copy(nid.szTip[:], tip)

	r, _, err := procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
	if r == 0 {
		return 0, fmt.Errorf("tray: Shell_NotifyIconW(NIM_ADD): %w", err)
	}
	return hwnd, nil
}

func (w *realWin32) UpdateTrayIcon(hwnd uintptr, tooltip string) error {
	tip, _ := windows.UTF16FromString(tooltip)
	var nid notifyIconDataW
	nid.cbSize = uint32(unsafe.Sizeof(nid))
	nid.hWnd = hwnd
	nid.uID = 1
	nid.uFlags = nifTip
	copy(nid.szTip[:], tip)

	r, _, err := procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
	if r == 0 {
		return fmt.Errorf("tray: Shell_NotifyIconW(NIM_MODIFY): %w", err)
	}
	return nil
}

func (w *realWin32) RemoveTrayIcon(hwnd uintptr) error {
	var nid notifyIconDataW
	nid.cbSize = uint32(unsafe.Sizeof(nid))
	nid.hWnd = hwnd
	nid.uID = 1

	r, _, err := procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
	if r == 0 {
		return fmt.Errorf("tray: Shell_NotifyIconW(NIM_DELETE): %w", err)
	}

	wndProcMu.Lock()
	delete(wndProcMap, hwnd)
	wndProcMu.Unlock()

	procDestroyWindow.Call(hwnd) //nolint:errcheck
	return nil
}

func (w *realWin32) ShowContextMenu(hwnd uintptr, items []MenuItem) (MenuItemID, error) {
	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))

	procSetForegroundWin.Call(hwnd)

	hMenu, _, err := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return 0, fmt.Errorf("tray: CreatePopupMenu: %w", err)
	}
	defer procDestroyMenu.Call(hMenu) //nolint:errcheck

	for _, item := range items {
		if item.Separator {
			procAppendMenu.Call(hMenu, mf_Separator, 0, 0) //nolint:errcheck
			continue
		}
		flags := uintptr(mf_String)
		if item.Disabled {
			flags |= mf_Disabled | mf_Grayed
		}
		labelPtr, _ := windows.UTF16PtrFromString(item.Label)
		procAppendMenu.Call(hMenu, flags, uintptr(item.ID), uintptr(unsafe.Pointer(labelPtr))) //nolint:errcheck
	}

	cmd, _, _ := procTrackPopupMenu.Call(
		hMenu,
		tpm_LeftButton|tpm_ReturnCmd|tpm_LeftAlign|tpm_NoNotify,
		uintptr(pt.x),
		uintptr(pt.y),
		0,
		hwnd,
		0,
	)
	return MenuItemID(cmd), nil
}

func (w *realWin32) PumpMessages(hwnd uintptr, quit <-chan struct{}, onCallback func(uint32, uintptr, uintptr)) {
	// Register a per-HWND callback that fires for tray messages.
	wndProcMu.Lock()
	wndProcMap[hwnd] = func(m uint32, wParam, lParam uintptr) {
		onCallback(m, wParam, lParam)
	}
	wndProcMu.Unlock()

	var m msg
	quitCh := make(chan struct{})

	// Monitor quit channel on a separate goroutine (cannot select in a message loop).
	go func() {
		select {
		case <-quit:
			procPostQuitMessage.Call(0)
		case <-quitCh:
		}
	}()

	for {
		r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 || r == ^uintptr(0) {
			// WM_QUIT or error → exit the loop.
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
	close(quitCh)
}

// ── TrayConfig — exported to cmd/engram ─────────────────────────────────────

// TrayConfig is the configuration passed to Run by cmd/engram/tray.go.
type TrayConfig struct {
	Port  int
	Token string
	DBDir string
}

// ── Run — public entrypoint ───────────────────────────────────────────────────

// Run initialises the tray icon, starts the message pump on a locked OS thread,
// and drives the worker goroutine. It blocks until the user clicks Quit.
func Run(cfg TrayConfig) error {
	w, err := newRealWin32()
	if err != nil {
		return fmt.Errorf("tray init: %w", err)
	}
	return runTray(cfg, w)
}

// runTray is the testable core: it accepts a win32 interface so tests can inject
// a fake without touching the Win32 layer.
func runTray(cfg TrayConfig, w win32) error {
	// Channel for worker actions (HTTP calls, browser launch).
	// Buffered so the pump goroutine never blocks on the worker.
	workCh := make(chan ActionFunc, 16)

	// quit is closed to signal the pump goroutine to exit the message loop.
	quit := make(chan struct{})

	// Mutable status snapshot, updated by the poller goroutine.
	var snapshotMu sync.Mutex
	snapshot := StatusSnapshot{DaemonRunning: true} // optimistic on start

	// Build action handlers that post HTTP calls to workCh.
	// CRITICAL: none of these handlers call win32 directly — they only enqueue
	// work to be executed by the worker goroutine. The pump goroutine dispatches
	// by sending to workCh; the worker goroutine executes.
	handlers := map[MenuItemID]ActionFunc{
		MenuIDOpenUI: func() {
			uiURL := fmt.Sprintf("http://127.0.0.1:%d/ui/?token=%s", cfg.Port, cfg.Token)
			log.Printf("tray: opening UI at http://127.0.0.1:%d/ui/", cfg.Port)
			if err := openBrowserFromTray(uiURL); err != nil {
				log.Printf("tray: open browser: %v", err)
			}
		},
		MenuIDConnect: func() {
			// Connect requires UI interaction — open the web UI for the user.
			uiURL := fmt.Sprintf("http://127.0.0.1:%d/ui/?token=%s", cfg.Port, cfg.Token)
			log.Printf("tray: opening UI for connect at http://127.0.0.1:%d/ui/", cfg.Port)
			if err := openBrowserFromTray(uiURL); err != nil {
				log.Printf("tray: open browser for connect: %v", err)
			}
		},
		MenuIDDisconnect: func() {
			if err := postControl(cfg, "/api/v1/central/disconnect", nil); err != nil {
				log.Printf("tray: disconnect: %v", err)
			}
		},
		MenuIDSyncNow: func() {
			if err := postControl(cfg, "/api/v1/sync/trigger", nil); err != nil {
				log.Printf("tray: sync trigger: %v", err)
			}
		},
		MenuIDQuit: func() {
			close(quit)
		},
	}
	disp := NewActionDispatcher(handlers)

	// Worker goroutine: consumes workCh and executes actions.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for fn := range workCh {
			fn()
		}
	}()

	// Status poller goroutine: polls GET /api/v1/status every 5 seconds.
	wg.Add(1)
	var hwndAtomic atomic.Uintptr
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ticker.C:
				st, err := getStatus(cfg)
				snapshotMu.Lock()
				if err != nil {
					snapshot.DaemonRunning = false
				} else {
					snapshot.DaemonRunning = true
					snapshot.Connected = st.CentralConnected
				}
				snapshotMu.Unlock()

				// Update tooltip.
				h := hwndAtomic.Load()
				if h != 0 {
					tooltip := "engram — disconnected"
					if snapshot.Connected {
						tooltip = "engram — connected"
					}
					_ = w.UpdateTrayIcon(h, tooltip)
				}
			}
		}
	}()

	// The message pump MUST run on a dedicated OS-thread-locked goroutine.
	// runtime.LockOSThread pins this goroutine to its current OS thread for
	// its entire lifetime — Win32 window/message APIs are thread-affine and
	// will misbehave if called from different OS threads.
	pumpErr := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		// LockOSThread is permanent for this goroutine; we do NOT call
		// runtime.UnlockOSThread — that would be incorrect (it is a stack-like
		// operation and the goroutine will exit when pump returns, freeing the
		// OS thread anyway).

		hwnd, err := w.RegisterTrayIcon(wm_TrayCallback)
		if err != nil {
			pumpErr <- err
			return
		}
		hwndAtomic.Store(hwnd)

		w.PumpMessages(hwnd, quit, func(m uint32, wParam, lParam uintptr) {
			// Called on the pump goroutine. Only Win32 calls here; no HTTP.
			if m == wm_TrayCallback {
				// lParam low word is the mouse event.
				// WM_RBUTTONUP (0x0205) or WM_LBUTTONDBLCLK (0x0203) → show menu.
				lw := lParam & 0xFFFF
				if lw == 0x0205 || lw == 0x0203 {
					snapshotMu.Lock()
					s := snapshot
					snapshotMu.Unlock()
					items := BuildMenu(s)
					id, err := w.ShowContextMenu(hwnd, items)
					if err != nil {
						log.Printf("tray: ShowContextMenu: %v", err)
						return
					}
					if id != 0 {
						disp.Dispatch(id, workCh)
					}
				}
			}
		})

		_ = w.RemoveTrayIcon(hwnd)
		pumpErr <- nil
	}()

	// Wait for the pump to exit (triggered by quit channel → WM_QUIT).
	pumpResult := <-pumpErr
	close(workCh)
	wg.Wait()
	return pumpResult
}

// openBrowserFromTray opens the default browser for url on Windows.
// It uses cmd /c start so it works from a tray process with no console.
func openBrowserFromTray(url string) error {
	// Use the same approach as ui.go's openBrowser.
	cmd := []string{"cmd", "/c", "start", url}
	return execDetached(cmd[0], cmd[1:]...)
}

// postControl issues an authenticated POST to a control API path.
// Called from the worker goroutine, never from the pump goroutine.
func postControl(cfg TrayConfig, path string, body any) error {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", cfg.Port, path)
	req, err := newControlRequest("POST", url, cfg.Token, cfg.Port, body)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("POST %s: daemon returned %d", path, resp.StatusCode)
	}
	return nil
}

// statusResponse is the subset of /api/v1/status we need for menu state.
type statusResponse struct {
	CentralConnected bool `json:"central_connected"`
}

// getStatus polls GET /api/v1/status and returns the connectivity state.
// Called from the poller goroutine, never from the pump goroutine.
func getStatus(cfg TrayConfig) (statusResponse, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/status", cfg.Port)
	req, err := newControlRequest("GET", url, cfg.Token, cfg.Port, nil)
	if err != nil {
		return statusResponse{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return statusResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusResponse{}, fmt.Errorf("status: %d", resp.StatusCode)
	}
	var st statusResponse
	if err := decodeJSON(resp, &st); err != nil {
		return statusResponse{}, err
	}
	return st, nil
}
