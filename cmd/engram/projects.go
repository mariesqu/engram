package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"text/tabwriter"

	"github.com/mariesqu/engram/internal/controlapi"
)

const projectsUsage = `Usage: engram projects <subcommand> [flags]

Manage per-project sync policies for the running engram resident daemon.

Subcommands:
  list                        List all known projects with their effective policy
  policy <project> <policy>   Set the policy for a project

Policies:
  synced      Observations are pushed to and pulled from central (default when central is configured)
  local-only  Observations are written locally only; push and pull are suppressed
  omitted     mem_save and mem_save_prompt refuse writes for this project

Flags (all subcommands):
  --db   Path to the local SQLite database (required; or set ENGRAM_DB)

Examples:
  engram projects list
  engram projects policy my-project local-only
  engram projects policy my-project synced
  engram projects policy my-project omitted
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
	default:
		return fmt.Errorf("projects: unknown subcommand %q; expected list or policy", args[0])
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

	if fs.NArg() != 2 {
		return fmt.Errorf("projects policy requires exactly two arguments: <project> <policy> (got %d)", fs.NArg())
	}

	project := fs.Arg(0)
	policyStr := fs.Arg(1)

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
