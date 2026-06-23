package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

// sessionUnstickProvider is the session-provider constructor used by
// "gc session unstick". It is a package-level seam so tests can inject a fake
// provider (mirrors dispatchControlSessionProvider).
var sessionUnstickProvider = newSessionProvider

// unstickTarget is one session that unstick will inspect.
type unstickTarget struct {
	id          string // session bead ID
	display     string // human-friendly label (alias, else session name, else ID)
	sessionName string // runtime/tmux session name
}

type sessionUnstickResult struct {
	SessionID string `json:"session_id"`
	Target    string `json:"target,omitempty"`
	Running   bool   `json:"running"`
	Parked    bool   `json:"parked"`
	Submitted bool   `json:"submitted"`
	Error     string `json:"error,omitempty"`
}

type sessionUnstickSummary struct {
	Scanned   int `json:"scanned"`
	Parked    int `json:"parked"`
	Submitted int `json:"submitted"`
}

type sessionUnstickJSON struct {
	SchemaVersion string                 `json:"schema_version"`
	DryRun        bool                   `json:"dry_run"`
	Results       []sessionUnstickResult `json:"results"`
	Summary       sessionUnstickSummary  `json:"summary"`
}

// newSessionUnstickCmd creates the "gc session unstick" command.
func newSessionUnstickCmd(stdout, stderr io.Writer) *cobra.Command {
	var all bool
	var dryRun bool
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "unstick [session-id-or-alias]",
		Short: "Submit input left parked in a session's input box",
		Long: `Detect and submit text left unsent in a session's input box.

After a restart or resume, a session can come back alive but with text sitting
unsubmitted in its input box — the agent looks frozen because it will not act
until the input is submitted. unstick detects that state (an idle prompt holding
non-whitespace text) and presses Enter to submit it.

Pass a session ID (e.g. gc-42) or alias (e.g. mayor) to target one session, or
--all to scan every running session. Use --dry-run to report parked sessions
without submitting anything.`,
		Example: `  gc session unstick mayor
  gc session unstick --all
  gc session unstick --all --dry-run`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionUnstick(args, all, dryRun, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeSessionIDs,
	}
	cmd.Flags().BoolVar(&all, "all", false, "scan all running sessions instead of a single target")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report parked sessions without submitting input")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL result")
	return cmd
}

// cmdSessionUnstick is the CLI entry point for "gc session unstick".
func cmdSessionUnstick(args []string, all, dryRun, jsonOutput bool, stdout, stderr io.Writer) int {
	if all && len(args) > 0 {
		fmt.Fprintln(stderr, "gc session unstick: cannot combine --all with a session argument") //nolint:errcheck // best-effort stderr
		return 1
	}
	if !all && len(args) == 0 {
		fmt.Fprintln(stderr, "gc session unstick: specify a session id/alias or --all") //nolint:errcheck // best-effort stderr
		return 1
	}

	store, code := openCityStore(stderr, "gc session unstick")
	if store == nil {
		return code
	}
	cityPath, err := resolveCity()
	var cfg *config.City
	if err == nil {
		cfg, _ = loadCityConfig(cityPath, stderr)
	}

	sp := sessionUnstickProvider()

	var targets []unstickTarget
	if all {
		targets, err = unstickAllTargets(store, sp)
		if err != nil {
			fmt.Fprintf(stderr, "gc session unstick: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	} else {
		id, rerr := resolveSessionIDWithConfig(cityPath, cfg, store, args[0])
		if rerr != nil {
			fmt.Fprintf(stderr, "gc session unstick: %v\n", rerr) //nolint:errcheck // best-effort stderr
			return 1
		}
		name, nerr := unstickSessionName(store, id)
		if nerr != nil {
			fmt.Fprintf(stderr, "gc session unstick: %v\n", nerr) //nolint:errcheck // best-effort stderr
			return 1
		}
		targets = []unstickTarget{{id: id, display: firstNonEmptyGCString(args[0], id), sessionName: name}}
	}

	results, err := collectUnstickResults(sp, targets, dryRun)
	if err != nil {
		fmt.Fprintf(stderr, "gc session unstick: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	return writeSessionUnstickOutput(results, dryRun, all, jsonOutput, stdout, stderr)
}

// collectUnstickResults inspects each target and, unless dryRun, submits parked
// input via SendKeys("Enter"). It is the testable core of "gc session unstick".
func collectUnstickResults(sp runtime.Provider, targets []unstickTarget, dryRun bool) ([]sessionUnstickResult, error) {
	pip, ok := sp.(runtime.ParkedInputProvider)
	if !ok {
		return nil, fmt.Errorf("session provider cannot detect parked input")
	}
	results := make([]sessionUnstickResult, 0, len(targets))
	for _, t := range targets {
		res := sessionUnstickResult{SessionID: t.id, Target: t.display}
		if t.sessionName == "" || !sp.IsRunning(t.sessionName) {
			results = append(results, res)
			continue
		}
		res.Running = true
		parked, perr := pip.ParkedInput(t.sessionName)
		if perr != nil {
			res.Error = perr.Error()
			results = append(results, res)
			continue
		}
		res.Parked = parked
		if parked && !dryRun {
			if serr := sp.SendKeys(t.sessionName, "Enter"); serr != nil {
				res.Error = serr.Error()
			} else {
				res.Submitted = true
			}
		}
		results = append(results, res)
	}
	return results, nil
}

// unstickAllTargets returns every running session as an unstick target.
func unstickAllTargets(store beads.Store, sp runtime.Provider) ([]unstickTarget, error) {
	allBeads, err := session.ListAllSessionBeads(store, beads.ListQuery{Sort: beads.SortCreatedDesc})
	if err != nil {
		return nil, err
	}
	var targets []unstickTarget
	for _, b := range allBeads {
		name := strings.TrimSpace(b.Metadata["session_name"])
		if name == "" || !sp.IsRunning(name) {
			continue
		}
		targets = append(targets, unstickTarget{
			id:          b.ID,
			display:     unstickDisplayName(b, name),
			sessionName: name,
		})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].display < targets[j].display })
	return targets, nil
}

// unstickSessionName returns the runtime session name recorded on the bead.
func unstickSessionName(store beads.Store, id string) (string, error) {
	b, err := store.Get(id)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(b.Metadata["session_name"]), nil
}

func unstickDisplayName(b beads.Bead, sessionName string) string {
	if alias := strings.TrimSpace(b.Metadata["alias"]); alias != "" {
		return alias
	}
	if sessionName != "" {
		return sessionName
	}
	return b.ID
}

func writeSessionUnstickOutput(results []sessionUnstickResult, dryRun, all, jsonOutput bool, stdout, stderr io.Writer) int {
	summary := sessionUnstickSummary{Scanned: len(results)}
	for _, r := range results {
		if r.Parked {
			summary.Parked++
		}
		if r.Submitted {
			summary.Submitted++
		}
	}

	if jsonOutput {
		if err := writeCLIJSONLine(stdout, sessionUnstickJSON{
			SchemaVersion: "1",
			DryRun:        dryRun,
			Results:       results,
			Summary:       summary,
		}); err != nil {
			fmt.Fprintf(stderr, "gc session unstick: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}

	for _, r := range results {
		fmt.Fprintf(stdout, "%s: %s\n", unstickResultLabel(r), unstickResultStatus(r, dryRun)) //nolint:errcheck // best-effort stdout
	}
	if all {
		fmt.Fprintf(stdout, "Scanned %d running session(s): %d parked, %d submitted.\n", summary.Scanned, summary.Parked, summary.Submitted) //nolint:errcheck // best-effort stdout
	}
	return 0
}

func unstickResultLabel(r sessionUnstickResult) string {
	if r.Target != "" && r.Target != r.SessionID {
		return fmt.Sprintf("%s (%s)", r.Target, r.SessionID)
	}
	return r.SessionID
}

func unstickResultStatus(r sessionUnstickResult, dryRun bool) string {
	switch {
	case r.Error != "":
		return "error: " + r.Error
	case !r.Running:
		return "not running — skipped"
	case r.Submitted:
		return "parked — submitted Enter"
	case r.Parked && dryRun:
		return "parked (dry-run, not submitted)"
	case r.Parked:
		return "parked — submit failed"
	default:
		return "no parked input"
	}
}
