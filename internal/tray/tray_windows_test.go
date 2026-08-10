//go:build windows

package tray

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// ── fakeWin32 — test double for win32 interface ───────────────────────────────

// fakeWin32 records calls without touching the Win32 desktop layer.
type fakeWin32 struct {
	mu sync.Mutex

	registerCalled bool
	removeCalled   bool
	updateCalls    []string
	balloons       []string

	// nextMenuSelect is the MenuItemID that ShowContextMenu returns.
	nextMenuSelect MenuItemID
	// menuItems is the last set of items passed to ShowContextMenu.
	menuItems []MenuItem
}

func newFakeWin32(selectID MenuItemID) *fakeWin32 {
	return &fakeWin32{nextMenuSelect: selectID}
}

func (f *fakeWin32) RegisterTrayIcon(_ uint32) (uintptr, error) {
	f.mu.Lock()
	f.registerCalled = true
	f.mu.Unlock()
	return 0x1234, nil
}

func (f *fakeWin32) UpdateTrayIcon(_ uintptr, tooltip string) error {
	f.mu.Lock()
	f.updateCalls = append(f.updateCalls, tooltip)
	f.mu.Unlock()
	return nil
}

func (f *fakeWin32) RemoveTrayIcon(_ uintptr) error {
	f.mu.Lock()
	f.removeCalled = true
	f.mu.Unlock()
	return nil
}

func (f *fakeWin32) ShowBalloon(_ uintptr, title, message string) error {
	f.mu.Lock()
	f.balloons = append(f.balloons, title+": "+message)
	f.mu.Unlock()
	return nil
}

func (f *fakeWin32) ShowContextMenu(_ uintptr, items []MenuItem) (MenuItemID, error) {
	f.mu.Lock()
	f.menuItems = make([]MenuItem, len(items))
	copy(f.menuItems, items)
	id := f.nextMenuSelect
	f.mu.Unlock()
	return id, nil
}

// PumpMessages simulates the Win32 message pump without system calls.
// It immediately fires a right-click callback, then blocks until quit is closed.
func (f *fakeWin32) PumpMessages(_ uintptr, quit <-chan struct{}, onCallback func(uint32, uintptr, uintptr)) {
	// lParam 0x0205 = WM_RBUTTONUP low word → show menu.
	onCallback(wm_TrayCallback, 0, 0x0205)

	select {
	case <-quit:
	case <-time.After(5 * time.Second):
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestTrayRun_Quit_StopsPump verifies that clicking Quit closes the quit channel
// and causes runTray to return without error.
func TestTrayRun_Quit_StopsPump(t *testing.T) {
	fake := newFakeWin32(MenuIDQuit)
	cfg := TrayConfig{Port: 17700, Token: "testtoken", DBDir: t.TempDir()}

	done := make(chan error, 1)
	go func() {
		done <- runTray(cfg, fake)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runTray returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runTray did not return after Quit action")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !fake.registerCalled {
		t.Error("RegisterTrayIcon was not called")
	}
	if !fake.removeCalled {
		t.Error("RemoveTrayIcon was not called (cleanup missing)")
	}
}

// TestTrayRun_SyncNow_CallsAPI verifies that selecting Sync Now triggers
// a POST to /api/v1/sync/trigger.
func TestTrayRun_SyncNow_CallsAPI(t *testing.T) {
	syncCalled := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/status":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"central_connected":true,"central_configured":true,"daemon_version":"0.1.0"}`))
		case "/api/v1/sync/trigger":
			w.WriteHeader(http.StatusAccepted)
			select {
			case syncCalled <- struct{}{}:
			default:
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	var port int
	fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &port)

	// Sequence: SyncNow (first right-click) → Quit (second right-click).
	seq := &sequencedFakeWin32{ids: []MenuItemID{MenuIDSyncNow, MenuIDQuit}}
	cfg := TrayConfig{Port: port, Token: "tok", DBDir: t.TempDir()}

	done := make(chan error, 1)
	go func() {
		done <- runTray(cfg, seq)
	}()

	select {
	case <-syncCalled:
		// Good — sync was triggered.
	case <-time.After(5 * time.Second):
		t.Error("Sync Now did not call /api/v1/sync/trigger")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runTray returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runTray did not return after Quit")
	}
}

// TestTrayRun_OpenUI_LaunchesBrowser verifies that selecting Open UI does not
// return an error (the browser launch is best-effort; we just ensure no panic).
// Full browser launch is manual-only.
func TestTrayRun_OpenUI_LaunchesBrowser(t *testing.T) {
	// Open UI calls openBrowserFromTray → exec.Command("cmd", "/c", "start", url).
	// We cannot test the actual browser open headlessly, so we verify that
	// the action is dispatched through the channel (not executed on the pump).
	ch := make(chan ActionFunc, 1)
	called := false
	disp := NewActionDispatcher(map[MenuItemID]ActionFunc{
		MenuIDOpenUI: func() { called = true },
	})
	disp.Dispatch(MenuIDOpenUI, ch)
	if len(ch) != 1 {
		t.Fatal("Open UI action was not enqueued")
	}
	(<-ch)() // execute on this goroutine (simulating worker)
	if !called {
		t.Error("Open UI action was not executed")
	}
}

// TestMenuAction_Quit_StopsPump is covered by TestTrayRun_Quit_StopsPump above.
// This test additionally verifies the menu model used when running.
func TestMenuAction_Quit_StopsPump(t *testing.T) {
	fake := newFakeWin32(MenuIDQuit)
	cfg := TrayConfig{Port: 17701, Token: "tok2", DBDir: t.TempDir()}

	done := make(chan error, 1)
	go func() { done <- runTray(cfg, fake) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Quit")
	}

	fake.mu.Lock()
	items := fake.menuItems
	fake.mu.Unlock()

	if findItem(items, MenuIDQuit) == nil {
		t.Error("Quit item not found in the rendered menu")
	}
}

// ── sequencedFakeWin32 ────────────────────────────────────────────────────────

// sequencedFakeWin32 returns menu IDs from a fixed sequence, firing a
// right-click callback every 50ms until quit is closed.
type sequencedFakeWin32 struct {
	mu  sync.Mutex
	ids []MenuItemID
	idx int
}

func (f *sequencedFakeWin32) RegisterTrayIcon(_ uint32) (uintptr, error) { return 0x1234, nil }
func (f *sequencedFakeWin32) UpdateTrayIcon(_ uintptr, _ string) error   { return nil }
func (f *sequencedFakeWin32) RemoveTrayIcon(_ uintptr) error             { return nil }
func (f *sequencedFakeWin32) ShowBalloon(_ uintptr, _, _ string) error   { return nil }

func (f *sequencedFakeWin32) ShowContextMenu(_ uintptr, _ []MenuItem) (MenuItemID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.idx >= len(f.ids) {
		return 0, nil
	}
	id := f.ids[f.idx]
	f.idx++
	return id, nil
}

func (f *sequencedFakeWin32) PumpMessages(_ uintptr, quit <-chan struct{}, onCallback func(uint32, uintptr, uintptr)) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-quit:
			return
		case <-ticker.C:
			onCallback(wm_TrayCallback, 0, 0x0205)
		}
	}
}

// ── Mixed-version daemon compatibility ─────────────────────────────────────────

// TestApplyStatus_LegacyDaemon_TreatsConnectedAsConfigured decodes a status
// body shaped like an OLD daemon's response — central_connected=true and no
// central_configured key at all — and verifies the resulting snapshot is
// self-consistent: Configured must be true (never Connected=true with
// Configured=false), and BuildMenu must render "Disconnect from central"
// plus an enabled "Sync Now" rather than the contradictory "Connected" +
// "Connect to central" + disabled "Sync Now" combination.
func TestApplyStatus_LegacyDaemon_TreatsConnectedAsConfigured(t *testing.T) {
	legacyBody := []byte(`{"central_connected":true}`) // no central_configured key

	var st statusResponse
	if err := json.Unmarshal(legacyBody, &st); err != nil {
		t.Fatalf("unmarshal legacy status body: %v", err)
	}
	if st.CentralConfigured {
		t.Fatal("test setup invalid: central_configured must decode false when absent")
	}

	snap := StatusSnapshot{DaemonRunning: true}
	applyStatus(&snap, st)

	if !snap.Connected {
		t.Error("Connected = false, want true")
	}
	if !snap.Configured {
		t.Error("Configured = false, want true (legacy connected=true implies configured)")
	}

	items := BuildMenu(snap)
	if item := findItem(items, MenuIDStatus); item == nil || item.Label != "Connected" {
		t.Errorf("status label = %+v, want \"Connected\"", item)
	}
	if findItem(items, MenuIDDisconnect) == nil {
		t.Error("menu missing \"Disconnect from central\" — legacy status must not show \"Connect to central\"")
	}
	if findItem(items, MenuIDConnect) != nil {
		t.Error("menu unexpectedly shows \"Connect to central\" for a legacy-connected daemon")
	}
	syncItem := findItem(items, MenuIDSyncNow)
	if syncItem == nil {
		t.Fatal("Sync Now item not found")
	}
	if syncItem.Disabled {
		t.Error("Sync Now is disabled, want enabled — a configured daemon must allow manual sync")
	}
}

// TestApplyStatus_NewDaemon_RoundTrips verifies a NEW daemon's status body
// (both fields present) maps through unchanged, so the legacy fallback in
// applyStatus never masks a genuine "connected but not configured" case —
// which cannot happen on a new daemon anyway, since central_connected can
// only be true when central_configured is true — nor a genuine
// "not connected, but configured" (Sync Error) case.
func TestApplyStatus_NewDaemon_RoundTrips(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		wantConnected  bool
		wantConfigured bool
	}{
		{
			name:           "connected and configured",
			body:           `{"central_connected":true,"central_configured":true}`,
			wantConnected:  true,
			wantConfigured: true,
		},
		{
			name:           "configured but sync failing",
			body:           `{"central_connected":false,"central_configured":true}`,
			wantConnected:  false,
			wantConfigured: true,
		},
		{
			name:           "never configured",
			body:           `{"central_connected":false,"central_configured":false}`,
			wantConnected:  false,
			wantConfigured: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var st statusResponse
			if err := json.Unmarshal([]byte(tt.body), &st); err != nil {
				t.Fatalf("unmarshal status body: %v", err)
			}
			var snap StatusSnapshot
			applyStatus(&snap, st)
			if snap.Connected != tt.wantConnected {
				t.Errorf("Connected = %v, want %v", snap.Connected, tt.wantConnected)
			}
			if snap.Configured != tt.wantConfigured {
				t.Errorf("Configured = %v, want %v", snap.Configured, tt.wantConfigured)
			}
		})
	}
}
