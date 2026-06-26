//go:build windows

package tray

import (
	"sync"
	"testing"
)

// ── BuildMenu model tests ─────────────────────────────────────────────────────

func TestBuildMenu_Connected_HasDisconnect(t *testing.T) {
	items := BuildMenu(StatusSnapshot{Connected: true, DaemonRunning: true})

	if items[0].Label != "Connected" {
		t.Errorf("status label = %q, want Connected", items[0].Label)
	}
	if !items[0].Disabled {
		t.Error("status label must be disabled")
	}

	assertMenuHasID(t, items, MenuIDDisconnect, "Disconnect from central")
	assertMenuLacksID(t, items, MenuIDConnect)

	syncItem := findItem(items, MenuIDSyncNow)
	if syncItem == nil {
		t.Fatal("Sync Now item missing")
	}
	if syncItem.Disabled {
		t.Error("Sync Now must be enabled when connected")
	}
}

func TestBuildMenu_Disconnected_HasConnect(t *testing.T) {
	items := BuildMenu(StatusSnapshot{Connected: false, DaemonRunning: true})

	if items[0].Label != "Disconnected" {
		t.Errorf("status label = %q, want Disconnected", items[0].Label)
	}

	assertMenuHasID(t, items, MenuIDConnect, "Connect to central")
	assertMenuLacksID(t, items, MenuIDDisconnect)

	syncItem := findItem(items, MenuIDSyncNow)
	if syncItem == nil {
		t.Fatal("Sync Now item missing")
	}
	if !syncItem.Disabled {
		t.Error("Sync Now must be disabled when disconnected")
	}
}

func TestBuildMenu_HasCheckForUpdates(t *testing.T) {
	for _, s := range []StatusSnapshot{
		{Connected: true, DaemonRunning: true},
		{Connected: false, DaemonRunning: true},
		{},
	} {
		items := BuildMenu(s)
		if findItem(items, MenuIDCheckUpdate) == nil {
			t.Errorf("Check for Updates item missing for snapshot %+v", s)
		}
	}
}

func TestBuildMenu_AlwaysHasQuit(t *testing.T) {
	for _, s := range []StatusSnapshot{
		{Connected: true, DaemonRunning: true},
		{Connected: false, DaemonRunning: false},
	} {
		items := BuildMenu(s)
		if findItem(items, MenuIDQuit) == nil {
			t.Errorf("Quit item missing for snapshot %+v", s)
		}
	}
}

func TestBuildMenu_DaemonNotRunning_OpenUIDisabled(t *testing.T) {
	items := BuildMenu(StatusSnapshot{Connected: false, DaemonRunning: false})
	ui := findItem(items, MenuIDOpenUI)
	if ui == nil {
		t.Fatal("Open UI item missing")
	}
	if !ui.Disabled {
		t.Error("Open UI must be disabled when daemon is not running")
	}
}

func TestBuildMenu_HasSeparatorBeforeQuit(t *testing.T) {
	items := BuildMenu(StatusSnapshot{Connected: false, DaemonRunning: true})
	quitIdx := -1
	for i, it := range items {
		if it.ID == MenuIDQuit {
			quitIdx = i
			break
		}
	}
	if quitIdx <= 0 {
		t.Fatal("Quit item not found")
	}
	if !items[quitIdx-1].Separator {
		t.Error("item before Quit must be a separator")
	}
}

// ── ActionDispatcher tests ────────────────────────────────────────────────────

func TestActionDispatcher_KnownID_SendsToChannel(t *testing.T) {
	ch := make(chan ActionFunc, 1)
	called := false
	disp := NewActionDispatcher(map[MenuItemID]ActionFunc{
		MenuIDSyncNow: func() { called = true },
	})

	disp.Dispatch(MenuIDSyncNow, ch)

	// The function must be in the channel, not yet called.
	if len(ch) != 1 {
		t.Fatalf("expected 1 item in channel, got %d", len(ch))
	}
	if called {
		t.Error("action must not be called on the dispatch (pump) goroutine")
	}

	// Simulate worker consuming and executing.
	fn := <-ch
	fn()
	if !called {
		t.Error("action not executed when consumed from channel")
	}
}

func TestActionDispatcher_UnknownID_NoSend(t *testing.T) {
	ch := make(chan ActionFunc, 1)
	disp := NewActionDispatcher(map[MenuItemID]ActionFunc{
		MenuIDQuit: func() {},
	})
	disp.Dispatch(MenuIDOpenUI, ch) // unknown
	if len(ch) != 0 {
		t.Error("unknown ID must not send to channel")
	}
}

func TestActionDispatcher_FullChannel_DoesNotBlock(t *testing.T) {
	// Unbuffered channel: any blocking send would deadlock the test.
	// Uses a NON-quit action — MenuIDQuit takes the synchronous path and never
	// touches the channel, so it cannot prove the drop-on-full contract.
	ch := make(chan ActionFunc) // unbuffered
	disp := NewActionDispatcher(map[MenuItemID]ActionFunc{
		MenuIDSyncNow: func() {},
	})
	// If Dispatch blocks, this test will be caught by go test -timeout.
	// The select in Dispatch must fall through to the default branch.
	done := make(chan struct{})
	go func() {
		defer close(done)
		disp.Dispatch(MenuIDSyncNow, ch)
	}()
	<-done // must complete without blocking
}

// TestActionDispatcher_ExecutionIsOffCallerGoroutine proves the decoupling
// contract: the ActionFunc passed through the channel is executed by whoever
// reads from the channel, NOT by the goroutine that called Dispatch. We verify
// this by consuming the channel in a separate goroutine and asserting the
// execution order: Dispatch returns before the action runs.
func TestActionDispatcher_ExecutionIsOffCallerGoroutine(t *testing.T) {
	ch := make(chan ActionFunc, 1)

	// Track whether Dispatch returned before the action ran.
	dispatchReturned := false
	actionRan := make(chan struct{})

	disp := NewActionDispatcher(map[MenuItemID]ActionFunc{
		MenuIDSyncNow: func() {
			// By the time this runs, Dispatch() must have already returned.
			if !dispatchReturned {
				t.Error("action ran before Dispatch returned — execution not decoupled")
			}
			close(actionRan)
		},
	})

	disp.Dispatch(MenuIDSyncNow, ch)
	dispatchReturned = true // set AFTER Dispatch returns

	// Worker goroutine consumes and executes.
	go func() {
		fn := <-ch
		fn()
	}()

	<-actionRan // wait for the worker to run the action
}

// ── helpers ───────────────────────────────────────────────────────────────────

func findItem(items []MenuItem, id MenuItemID) *MenuItem {
	for i := range items {
		if items[i].ID == id {
			return &items[i]
		}
	}
	return nil
}

func assertMenuHasID(t *testing.T, items []MenuItem, id MenuItemID, wantLabel string) {
	t.Helper()
	it := findItem(items, id)
	if it == nil {
		t.Errorf("menu item %d (%q) missing", id, wantLabel)
		return
	}
	if it.Label != wantLabel {
		t.Errorf("item %d label = %q, want %q", id, it.Label, wantLabel)
	}
}

func assertMenuLacksID(t *testing.T, items []MenuItem, id MenuItemID) {
	t.Helper()
	if findItem(items, id) != nil {
		t.Errorf("menu item %d should not be present", id)
	}
}

// TestActionDispatcher_QuitNeverDropped: Quit executes synchronously even when
// the work channel is completely full — a droppable Quit would leave a tray
// that cannot be exited once the worker is backlogged.
func TestActionDispatcher_QuitNeverDropped(t *testing.T) {
	quitRan := false
	d := NewActionDispatcher(map[MenuItemID]ActionFunc{
		MenuIDQuit:    func() { quitRan = true },
		MenuIDSyncNow: func() {},
	})

	// Fill the channel to capacity so any channel send would drop.
	workCh := make(chan ActionFunc, 2)
	d.Dispatch(MenuIDSyncNow, workCh)
	d.Dispatch(MenuIDSyncNow, workCh)

	d.Dispatch(MenuIDQuit, workCh)
	if !quitRan {
		t.Error("Quit was dropped on a full channel — tray would be unexitable")
	}
	if len(workCh) != 2 {
		t.Errorf("Quit went through the channel (len=%d), want synchronous execution", len(workCh))
	}
}

// TestActionDispatcher_DoubleQuit_NoPanic: a second Quit (double click) must
// not panic — the handler guards close(quit) with sync.Once in runTray; here
// we prove Dispatch itself happily invokes the handler twice.
func TestActionDispatcher_DoubleQuit_NoPanic(t *testing.T) {
	quit := make(chan struct{})
	var once sync.Once
	d := NewActionDispatcher(map[MenuItemID]ActionFunc{
		MenuIDQuit: func() { once.Do(func() { close(quit) }) },
	})
	workCh := make(chan ActionFunc, 1)
	d.Dispatch(MenuIDQuit, workCh) // closes quit
	d.Dispatch(MenuIDQuit, workCh) // must be a no-op, not a panic
	select {
	case <-quit:
	default:
		t.Error("quit channel not closed")
	}
}
