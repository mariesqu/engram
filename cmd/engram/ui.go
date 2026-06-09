package main

import (
	"errors"
	"flag"
	"fmt"
	"os/exec"
	"runtime"
)

const uiUsage = `Usage: engram ui [--db <path>]

Open the engram web UI in the default browser.

Requires the resident daemon to be running (engram daemon --http). If no
daemon is running, prints the URL and exits with an error.

Flags:
  --db   Path to the local SQLite database (required; or set ENGRAM_DB)
`

// runUICmd is the entry point for `engram ui`.
func runUICmd(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), uiUsage) }

	db := fs.String("db", "", "path to local SQLite database (required; or set ENGRAM_DB)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("ui takes no positional arguments; unexpected: %v", fs.Args())
	}

	if *db == "" {
		*db = envOr("ENGRAM_DB", "")
	}
	if *db == "" {
		return fmt.Errorf("--db is required (or set ENGRAM_DB)")
	}

	client, err := NewControlClient(daemonDir(*db))
	if err != nil {
		return err
	}

	// Include the bearer token in the URL so the browser can exchange it for a
	// session cookie via the /ui/?token= exchange endpoint (PR-④).
	uiURL := fmt.Sprintf("http://127.0.0.1:%d/ui/?token=%s", client.port, client.token)
	fmt.Printf("Opening: %s\n", uiURL)

	if openErr := openBrowser(uiURL); openErr != nil {
		// Non-fatal: print the URL so the user can open it manually.
		fmt.Printf("Could not open browser automatically: %v\n", openErr)
		fmt.Printf("Open this URL in your browser: %s\n", uiURL)
	}
	return nil
}

// openBrowser launches the default browser for the given URL.
// It delegates to the platform-appropriate command:
//   - Windows:  cmd /c start <url>
//   - macOS:    open <url>
//   - Linux:    xdg-open <url>
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
