//go:build windows

package tray

// MenuItemID is the command identifier for a context-menu item.
// Values are passed to TrackPopupMenu and returned in WM_COMMAND.
type MenuItemID uint32

const (
	MenuIDStatus      MenuItemID = 1001 // disabled status label — never sent as WM_COMMAND
	MenuIDOpenUI      MenuItemID = 1002
	MenuIDConnect     MenuItemID = 1003 // "Connect to central" (disconnected state)
	MenuIDDisconnect  MenuItemID = 1004 // "Disconnect from central" (connected state)
	MenuIDSyncNow     MenuItemID = 1005
	MenuIDQuit        MenuItemID = 1006
	MenuIDCheckUpdate MenuItemID = 1007 // "Check for Updates"
)

// MenuItem describes one entry in the tray context menu.
type MenuItem struct {
	ID        MenuItemID
	Label     string
	Disabled  bool // grayed out; receives no WM_COMMAND
	Separator bool // true → draw a horizontal rule; Label/ID/Disabled ignored
}

// StatusSnapshot is the last-known status polled from GET /api/v1/status.
// It is the sole input to BuildMenu — keeping the menu model pure and testable
// without any Win32 or HTTP calls.
type StatusSnapshot struct {
	// Connected is true when the daemon is connected to a central server.
	Connected bool
	// DaemonRunning is true when we have successfully reached the daemon.
	DaemonRunning bool
}

// BuildMenu constructs the ordered list of menu items from the current status.
// It is a pure function — the same snapshot always produces the same menu — so
// it is fully testable without Win32 calls.
//
// Menu order (per spec):
//  1. Status label (disabled)
//  2. Open UI
//  3. Connect / Disconnect (context-sensitive)
//  4. Sync Now (disabled when not connected)
//  5. Check for Updates
//  6. Separator
//  7. Quit
func BuildMenu(s StatusSnapshot) []MenuItem {
	statusLabel := "Disconnected"
	if s.Connected {
		statusLabel = "Connected"
	}

	items := []MenuItem{
		{ID: MenuIDStatus, Label: statusLabel, Disabled: true},
		{ID: MenuIDOpenUI, Label: "Open UI", Disabled: !s.DaemonRunning},
	}

	if s.Connected {
		items = append(items, MenuItem{ID: MenuIDDisconnect, Label: "Disconnect from central"})
	} else {
		items = append(items, MenuItem{ID: MenuIDConnect, Label: "Connect to central"})
	}

	items = append(items,
		MenuItem{ID: MenuIDSyncNow, Label: "Sync Now", Disabled: !s.Connected},
		MenuItem{ID: MenuIDCheckUpdate, Label: "Check for Updates"},
		MenuItem{Separator: true},
		MenuItem{ID: MenuIDQuit, Label: "Quit"},
	)
	return items
}

// ActionFunc is a no-argument function executed by the worker goroutine when a
// menu item is selected. HTTP calls, browser launches, and other side-effects
// happen here — never on the message-pump thread.
type ActionFunc func()

// ActionDispatcher maps MenuItemIDs to ActionFuncs.
// It is the bridge between the Win32 WM_COMMAND message and the worker goroutine.
type ActionDispatcher struct {
	handlers map[MenuItemID]ActionFunc
}

// NewActionDispatcher constructs an ActionDispatcher with the given handlers.
// Unknown IDs are silently ignored.
func NewActionDispatcher(handlers map[MenuItemID]ActionFunc) *ActionDispatcher {
	return &ActionDispatcher{handlers: handlers}
}

// Dispatch looks up the handler for id and, if found, sends it to workCh for
// execution on the worker goroutine. The send is non-blocking: if the channel
// is full the action is dropped (prevents pump blocking on a backlogged worker).
//
// EXCEPTION: MenuIDQuit executes SYNCHRONOUSLY on the calling goroutine and is
// never droppable. Its handler only closes the quit channel (no HTTP, no
// blocking) — and a Quit silently dropped because the worker is backlogged
// would leave a tray that cannot be exited.
func (d *ActionDispatcher) Dispatch(id MenuItemID, workCh chan<- ActionFunc) {
	fn, ok := d.handlers[id]
	if !ok {
		return
	}
	if id == MenuIDQuit {
		fn()
		return
	}
	select {
	case workCh <- fn:
	default:
		// Worker is busy — drop rather than block the pump thread.
	}
}
