package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"text/tabwriter"

	"github.com/mariesqu/engram/internal/centralstore"
	"github.com/mariesqu/engram/internal/controlapi"
)

const projectsUsage = `Usage: engram projects <subcommand> [flags]

Manage per-project sync policies for the running engram resident daemon.

Subcommands:
  list                        List all known projects with their effective policy
  policy <project> <policy>   Set the policy for a project
  delete <project> [flags]    Delete a project (one or more modes required)

Policies:
  synced      Observations are pushed to and pulled from central (default when central is configured)
  local-only  Observations are written locally only; push and pull are suppressed
  omitted     mem_save and mem_save_prompt refuse writes for this project

Delete flags:
  --local              Hard-delete local data and set policy to omitted (needs --db)
  --remote=unshare     Hard-delete from central without tombstones (needs --dsn; admin op)
  --remote=purge-all   Tombstone all live memories so deletes propagate to all nodes (needs --db)
  --db PATH            Path to local SQLite database (or set ENGRAM_DB)
  --dsn DSN            Postgres DSN for central (or set ENGRAM_DSN; used by --remote=unshare)
  --yes                Confirm destructive operation (required)

Examples:
  engram projects list
  engram projects policy my-project local-only
  engram projects policy my-project synced
  engram projects policy my-project omitted
  engram projects delete my-project --local --yes
  engram projects delete my-project --remote=purge-all --yes
  engram projects delete my-project --remote=unshare --dsn "postgres://..." --yes
  engram projects delete my-project --local --remote=unshare --dsn "postgres://..." --yes
`

// runProjectsCmd is the entry point for `engram projects`.
func runProjectsCmd(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(projectsUsage)
		return nil
	}

	switch args[0] {
	case "list":
		return runProjectsListCmd(args[1:])
	case "policy":
		return runProjectsPolicyCmd(args[1:])
	case "delete":
		return runProjectsDeleteCmd(args[1:])
	default:
		return fmt.Errorf("projects: unknown subcommand %q; expected list, policy, or delete", args[0])
	}
}

// runProjectsListCmd implements `engram projects list`.
func runProjectsListCmd(args []string) error {
	fs := flag.NewFlagSet("projects list", flag.ContinueOnError)
	fs.Usage = func() { fmt.Print(projectsUsage) }
	db := fs.String("db", "", "path to local SQLite database (required; or set ENGRAM_DB)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("projects list takes no positional arguments; unexpected: %v", fs.Args())
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

	var projects []controlapi.ProjectPolicy
	if err := client.Get("/api/v1/projects", &projects); err != nil {
		if errors.Is(err, ErrDaemonNotRunning) {
			return fmt.Errorf("engram daemon is not running: %w", err)
		}
		return fmt.Errorf("projects list: %w", err)
	}

	if len(projects) == 0 {
		fmt.Println("(no projects)")
		return nil
	}

	// Print as an aligned table.
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tPOLICY")
	for _, p := range projects {
		fmt.Fprintf(tw, "%s\t%s\n", p.Name, p.Policy)
	}
	return tw.Flush()
}

// runProjectsPolicyCmd implements `engram projects policy <project> <policy>`.
func runProjectsPolicyCmd(args []string) error {
	fs := flag.NewFlagSet("projects policy", flag.ContinueOnError)
	fs.Usage = func() { fmt.Print(projectsUsage) }
	db := fs.String("db", "", "path to local SQLite database (required; or set ENGRAM_DB)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	// <project> <policy> are positionals; Go's flag package stops at the first
	// one, so parse any flags that FOLLOW them with a second pass — both
	// `projects policy --db X p synced` and `projects policy p synced --db X` work.
	rest := fs.Args()
	if len(rest) < 2 {
		return fmt.Errorf("projects policy requires exactly two arguments: <project> <policy>")
	}
	project := rest[0]
	policyStr := rest[1]
	if err := fs.Parse(rest[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("projects policy takes exactly <project> <policy>; unexpected: %v", fs.Args())
	}

	// Validate policy value before making a network call.
	if _, err := controlapi.ParsePolicy(policyStr); err != nil {
		return fmt.Errorf("projects policy: invalid policy %q: %s", policyStr, err)
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

	// PathEscape: a project name containing "/" (or other reserved chars) must
	// arrive at the {project} route segment as ONE segment, not split the path.
	path := fmt.Sprintf("/api/v1/projects/%s/policy", url.PathEscape(project))
	body := map[string]string{"policy": policyStr}
	if err := client.Put(path, body, nil); err != nil {
		if errors.Is(err, ErrDaemonNotRunning) {
			return fmt.Errorf("engram daemon is not running: %w", err)
		}
		return fmt.Errorf("projects policy: %w", err)
	}

	fmt.Printf("policy for %q set to %q\n", project, policyStr)
	return nil
}

// runProjectsDeleteCmd implements `engram projects delete <project> [flags]`.
//
// Flag parsing follows the two-parse pattern from memories.go (cmd/engram):
// Go's flag package stops parsing at the first positional argument, so the
// project name is extracted after the first parse and the remaining args are
// re-parsed as trailing flags.
func runProjectsDeleteCmd(args []string) error {
	fs := flag.NewFlagSet("projects delete", flag.ContinueOnError)
	fs.Usage = func() { fmt.Print(projectsUsage) }

	local := fs.Bool("local", false, "hard-delete local data and set policy to omitted (needs --db)")
	remote := fs.String("remote", "", "remote operation: unshare (admin, needs --dsn) or purge-all (needs --db)")
	db := fs.String("db", "", "path to local SQLite database (or set ENGRAM_DB)")
	dsn := fs.String("dsn", "", "Postgres DSN for central (or set ENGRAM_DSN; used by --remote=unshare)")
	yes := fs.Bool("yes", false, "confirm destructive operation")

	// First parse: leading flags before the positional <project>.
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	rest := fs.Args()
	if len(rest) == 0 {
		return fmt.Errorf("projects delete requires a <project> argument")
	}
	project := rest[0]

	// Second parse: trailing flags after the positional argument.
	if err := fs.Parse(rest[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("projects delete takes exactly one positional argument; unexpected: %v", fs.Args())
	}

	// Resolve env vars after flag parse (avoids leaking secrets into defaults).
	if *db == "" {
		*db = envOr("ENGRAM_DB", "")
	}
	if *dsn == "" {
		*dsn = envOr("ENGRAM_DSN", "")
	}

	// Validate that at least one mode is selected.
	if !*local && *remote == "" {
		return fmt.Errorf("projects delete: specify at least --local or --remote=unshare|purge-all")
	}

	// Validate remote value.
	if *remote != "" && *remote != "unshare" && *remote != "purge-all" {
		return fmt.Errorf("projects delete: --remote must be unshare or purge-all (got %q)", *remote)
	}

	// Check required flags per mode.
	if *local && *db == "" {
		return fmt.Errorf("projects delete --local: --db is required (or set ENGRAM_DB)")
	}
	if *remote == "purge-all" && *db == "" {
		return fmt.Errorf("projects delete --remote=purge-all: --db is required (or set ENGRAM_DB)")
	}
	if *remote == "unshare" && *dsn == "" {
		return fmt.Errorf("projects delete --remote=unshare: --dsn is required (or set ENGRAM_DSN)")
	}

	// Without --yes, print a dry-run summary and return.
	if !*yes {
		fmt.Printf("Dry run — would perform the following actions for project %q:\n", project)
		if *local {
			fmt.Println("  [local]       hard-delete all local data; set policy to omitted")
		}
		if *remote == "purge-all" {
			fmt.Println("  [purge-all]   tombstone all live memories; deletions propagate to all synced nodes")
		}
		if *remote == "unshare" {
			fmt.Println("  [unshare]     hard-delete from central WITHOUT tombstones (no propagation)")
			if *db != "" {
				fmt.Println("                + set local policy to local-only (because --db was also given)")
			}
		}
		fmt.Println("\nRe-run with --yes to execute.")
		return nil
	}

	// Execute: --local
	if *local {
		client, err := NewControlClient(daemonDir(*db))
		if err != nil {
			return fmt.Errorf("projects delete --local: %w", err)
		}
		path := fmt.Sprintf("/api/v1/projects/%s?scope=local", url.PathEscape(project))
		var result map[string]int
		if err := client.Delete(path); err != nil {
			return fmt.Errorf("projects delete --local: %w", err)
		}
		_ = result
		fmt.Printf("[local] hard-deleted local data for project %q and set policy to omitted\n", project)
	}

	// Execute: --remote=purge-all
	if *remote == "purge-all" {
		client, err := NewControlClient(daemonDir(*db))
		if err != nil {
			return fmt.Errorf("projects delete --remote=purge-all: %w", err)
		}
		path := fmt.Sprintf("/api/v1/projects/%s?scope=purge-all", url.PathEscape(project))
		if err := client.Delete(path); err != nil {
			return fmt.Errorf("projects delete --remote=purge-all: %w", err)
		}
		fmt.Printf("[purge-all] tombstoned all live memories for project %q; deletions will propagate via sync\n", project)
	}

	// Execute: --remote=unshare
	if *remote == "unshare" {
		ctx := context.Background()
		store, err := centralstore.Open(ctx, *dsn)
		if err != nil {
			return fmt.Errorf("projects delete --remote=unshare: open central: %w", err)
		}
		defer store.Close()

		n, err := store.DeleteProject(ctx, project)
		if err != nil {
			return fmt.Errorf("projects delete --remote=unshare: %w", err)
		}
		fmt.Printf("[unshare] deleted %d rows from central for project %q (no tombstones written)\n", n, project)

		// If --db was also given, set local policy to local-only via the daemon.
		if *db != "" {
			client, err := NewControlClient(daemonDir(*db))
			if err != nil {
				return fmt.Errorf("projects delete --remote=unshare: connect to daemon: %w", err)
			}
			policyPath := fmt.Sprintf("/api/v1/projects/%s/policy", url.PathEscape(project))
			body := map[string]string{"policy": "local-only"}
			if err := client.Put(policyPath, body, nil); err != nil {
				return fmt.Errorf("projects delete --remote=unshare: set local policy: %w", err)
			}
			fmt.Printf("[unshare] set local policy for %q to local-only (node keeps its copy, stops re-pushing)\n", project)
		}
	}

	return nil
}
