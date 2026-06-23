package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"text/tabwriter"
	"unicode/utf8"

	"github.com/mariesqu/engram/internal/controlapi"
)

const memoriesUsage = `Usage: engram memories <subcommand> [flags]

Browse and manage memories stored in the running engram resident daemon.

Subcommands:
  list                          List the most recent memories
  search <query>                Search memories using full-text search
  edit <id>                     Edit an existing memory (requires --title and --content)
  delete <id>                   Delete a memory by ID

Flags (list/search subcommands):
  --db       Path to the local SQLite database (required; or set ENGRAM_DB)
  --project  Filter by project name (optional)
  --limit    Maximum number of results (default 50, max 200)

Examples:
  engram memories list
  engram memories list --project my-project --limit 20
  engram memories search "authentication bug"
  engram memories search "auth" --project my-project
  engram memories edit 42 --title "New title" --content "Updated content"
  engram memories delete 42
`

// runMemoriesCmd is the entry point for `engram memories`.
func runMemoriesCmd(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(memoriesUsage)
		return nil
	}

	switch args[0] {
	case "list":
		return runMemoriesListCmd(args[1:])
	case "search":
		return runMemoriesSearchCmd(args[1:])
	case "edit":
		return runMemoriesEditCmd(args[1:])
	case "delete":
		return runMemoriesDeleteCmd(args[1:])
	default:
		return fmt.Errorf("memories: unknown subcommand %q; expected list, search, edit, or delete", args[0])
	}
}

// runMemoriesListCmd implements `engram memories list`.
func runMemoriesListCmd(args []string) error {
	fs := flag.NewFlagSet("memories list", flag.ContinueOnError)
	fs.Usage = func() { fmt.Print(memoriesUsage) }
	db := fs.String("db", "", "path to local SQLite database (required; or set ENGRAM_DB)")
	project := fs.String("project", "", "filter by project name")
	limit := fs.Int("limit", 50, "maximum number of results (max 200)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("memories list takes no positional arguments; unexpected: %v", fs.Args())
	}
	return doMemoriesRequest("", *project, *limit, *db)
}

// runMemoriesSearchCmd implements `engram memories search <query>`.
func runMemoriesSearchCmd(args []string) error {
	fs := flag.NewFlagSet("memories search", flag.ContinueOnError)
	fs.Usage = func() { fmt.Print(memoriesUsage) }
	db := fs.String("db", "", "path to local SQLite database (required; or set ENGRAM_DB)")
	project := fs.String("project", "", "filter by project name")
	limit := fs.Int("limit", 50, "maximum number of results (max 200)")
	// Parse leading flags, take the query, then parse any TRAILING flags too, so
	// both `search --project X "q"` and `search "q" --project X` work — Go's flag
	// package otherwise stops parsing at the first positional argument.
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return fmt.Errorf("memories search requires a query argument")
	}
	query := rest[0]
	if err := fs.Parse(rest[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("memories search takes exactly one query argument; unexpected: %v", fs.Args())
	}
	return doMemoriesRequest(query, *project, *limit, *db)
}

// doMemoriesRequest issues GET /api/v1/memories and prints the result table.
func doMemoriesRequest(query, project string, limit int, db string) error {
	if db == "" {
		db = envOr("ENGRAM_DB", "")
	}
	if db == "" {
		return fmt.Errorf("--db is required (or set ENGRAM_DB)")
	}

	client, err := NewControlClient(daemonDir(db))
	if err != nil {
		return err
	}

	params := url.Values{}
	params.Set("limit", fmt.Sprintf("%d", limit))
	if query != "" {
		params.Set("q", query)
	}
	if project != "" {
		params.Set("project", project)
	}
	path := "/api/v1/memories?" + params.Encode()

	var memories []controlapi.MemorySummary
	if err := client.Get(path, &memories); err != nil {
		if errors.Is(err, ErrDaemonNotRunning) {
			return fmt.Errorf("engram daemon is not running: %w", err)
		}
		return fmt.Errorf("memories: %w", err)
	}

	if len(memories) == 0 {
		fmt.Println("(no memories)")
		return nil
	}

	// Print as an aligned table.
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPROJECT\tTYPE\tTITLE")
	for _, m := range memories {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", m.ID, m.Project, m.Type, truncateTitle(m.Title, 60))
	}
	return tw.Flush()
}

// runMemoriesEditCmd implements `engram memories edit <id>`.
func runMemoriesEditCmd(args []string) error {
	fs := flag.NewFlagSet("memories edit", flag.ContinueOnError)
	fs.Usage = func() { fmt.Print(memoriesUsage) }
	var (
		title   = fs.String("title", "", "new title (required)")
		content = fs.String("content", "", "new content (required)")
		typ     = fs.String("type", "", "new type (optional; preserves existing when empty)")
		db      = fs.String("db", "", "path to local SQLite database (required; or set ENGRAM_DB)")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	// <id> is the first positional; Go's flag package stops parsing at it, so parse
	// any flags that follow the id with a second pass (so `edit <id> --title …` works).
	rest := fs.Args()
	if len(rest) < 1 {
		return fmt.Errorf("memories edit: usage: engram memories edit <id> --title T --content C [--type X] [--db PATH]")
	}
	id, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("memories edit: id must be a positive integer, got %q", rest[0])
	}
	if err := fs.Parse(rest[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *title == "" {
		return fmt.Errorf("memories edit: --title is required")
	}
	if *content == "" {
		return fmt.Errorf("memories edit: --content is required")
	}

	dbPath := *db
	if dbPath == "" {
		dbPath = envOr("ENGRAM_DB", "")
	}
	if dbPath == "" {
		return fmt.Errorf("--db is required (or set ENGRAM_DB)")
	}

	client, err := NewControlClient(daemonDir(dbPath))
	if err != nil {
		return fmt.Errorf("memories edit: connect to daemon: %w", err)
	}

	body := map[string]string{
		"title":   *title,
		"content": *content,
		"type":    *typ,
	}
	var result controlapi.MemorySummary
	if err := client.Put(fmt.Sprintf("/api/v1/memories/%d", id), body, &result); err != nil {
		return fmt.Errorf("memories edit: %w", err)
	}
	fmt.Printf("memory %d updated: %s\n", result.ID, result.Title)
	return nil
}

// runMemoriesDeleteCmd implements `engram memories delete <id>`.
func runMemoriesDeleteCmd(args []string) error {
	fs := flag.NewFlagSet("memories delete", flag.ContinueOnError)
	fs.Usage = func() { fmt.Print(memoriesUsage) }
	var (
		db  = fs.String("db", "", "path to local SQLite database (required; or set ENGRAM_DB)")
		yes = fs.Bool("yes", false, "skip confirmation prompt (currently non-interactive; always deletes)")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	// <id> is the first positional; parse any flags that follow it (second pass).
	rest := fs.Args()
	if len(rest) < 1 {
		return fmt.Errorf("memories delete: usage: engram memories delete <id> [--yes] [--db PATH]")
	}
	id, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("memories delete: id must be a positive integer, got %q", rest[0])
	}
	if err := fs.Parse(rest[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	_ = yes // flag accepted for future interactive confirmation; currently always deletes

	dbPath := *db
	if dbPath == "" {
		dbPath = envOr("ENGRAM_DB", "")
	}
	if dbPath == "" {
		return fmt.Errorf("--db is required (or set ENGRAM_DB)")
	}

	client, err := NewControlClient(daemonDir(dbPath))
	if err != nil {
		return fmt.Errorf("memories delete: connect to daemon: %w", err)
	}

	if err := client.Delete(fmt.Sprintf("/api/v1/memories/%d", id)); err != nil {
		return fmt.Errorf("memories delete: %w", err)
	}
	fmt.Printf("memory %d deleted\n", id)
	return nil
}

// truncateTitle truncates s to at most maxRunes runes, appending "…" when truncated.
func truncateTitle(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + "…"
}
