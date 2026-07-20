package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

func newHookCmd(stdout, stderr io.Writer) *cobra.Command {
	var inject bool
	var hookFormat string
	cmd := &cobra.Command{
		Use:   "hook [agent]",
		Short: "Check for available work",
		Long: `Checks for available work using the agent's work_query config.

Without --inject: prints normalized ready-only output, exits 0 if work exists, 1 if empty.
With --inject: silent legacy Stop-hook compatibility; skips the work query and always exits 0.

		The agent is determined from $GC_AGENT or a positional argument.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdHookWithFormat(args, inject, hookFormat, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&inject, "inject", false, "silent legacy Stop-hook compatibility; skip work query and exit 0")
	cmd.Flags().StringVar(&hookFormat, "hook-format", "", "format hook output for a provider")
	if flag := cmd.Flags().Lookup("hook-format"); flag != nil {
		flag.Hidden = true
	}
	return cmd
}

// cmdHook is the CLI entry point for gc hook. Resolves the agent from
// $GC_AGENT or a positional argument, loads the city config, and runs
// the agent's work query.
func cmdHook(args []string, stdout, stderr io.Writer) int {
	return cmdHookWithFormat(args, false, "", stdout, stderr)
}

func cmdHookWithFormat(args []string, inject bool, hookFormat string, stdout, stderr io.Writer) int {
	if inject {
		return 0
	}
	// Accepted for compatibility with installed hook commands; non-inject
	// gc hook output ignores provider-specific formatting.
	_ = hookFormat

	agentName := os.Getenv("GC_ALIAS")
	if agentName == "" {
		agentName = os.Getenv("GC_AGENT")
	}
	sessionTemplateContext := false
	if len(args) == 0 {
		template := strings.TrimSpace(os.Getenv("GC_TEMPLATE"))
		hasSessionContext := strings.TrimSpace(os.Getenv("GC_SESSION_NAME")) != "" ||
			strings.TrimSpace(os.Getenv("GC_SESSION_ID")) != ""
		if template != "" && hasSessionContext {
			agentName = template
			sessionTemplateContext = true
		}
	}
	if len(args) > 0 {
		agentName = args[0]
	}
	if agentName == "" {
		fmt.Fprintln(stderr, "gc hook: agent not specified (set $GC_AGENT or pass as argument)") //nolint:errcheck // best-effort stderr
		return 1
	}

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc hook: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// Normalize relative rig paths to absolute so downstream rig-matching
	// (agentCommandDir, bdRuntimeEnvForRig) compares apples to apples.
	// Other CLI entry points (cmd_sling, cmd_start, cmd_rig, cmd_supervisor)
	// do the same immediately after loadCityConfig.
	resolveRigPaths(cityPath, cfg.Rigs)

	if citySuspended(cfg) {
		fmt.Fprintln(stderr, "gc hook: city is suspended") //nolint:errcheck // best-effort stderr
		return 1
	}

	a, ok := resolveAgentIdentity(cfg, agentName, currentRigContext(cfg))
	if !ok {
		fmt.Fprintf(stderr, "gc hook: agent %q not found in config\n", agentName) //nolint:errcheck // best-effort stderr
		return 1
	}

	if isAgentEffectivelySuspended(cfg, &a) {
		fmt.Fprintf(stderr, "gc hook: agent %q is suspended\n", agentName) //nolint:errcheck // best-effort stderr
		return 1
	}

	cityName := loadedCityName(cfg, cityPath)
	workQuery := a.EffectiveWorkQuery()
	// Expand {{.Rig}}/{{.AgentBase}} in user-supplied work_query so agent-side
	// hook invocation sees the same rig substitution as the controller-side
	// probes in build_desired_state.go / session_reconcile.go. #793.
	workQuery = expandAgentCommandTemplate(cityPath, cityName, &a, cfg.Rigs, "work_query", workQuery, stderr)
	workDir := agentCommandDir(cityPath, &a, cfg.Rigs)

	// Build the work query subprocess environment. Rig-backed agents get
	// rig-scoped BEADS_DIR / GC_RIG_ROOT / Dolt coordinates so the query
	// reads the rig store rather than whatever BEADS_DIR the parent
	// process happens to inherit (issue #514). Many built-in work queries
	// also key off session identity. Explicit hook targets get resolved
	// names; named-session context preserves the runtime-supplied owner
	// env while selecting the backing config through GC_TEMPLATE.
	resolvedAgentName := a.QualifiedName()
	agentForQuery := resolvedAgentName
	sessionForQuery := ""
	if sessionTemplateContext {
		agentForQuery = os.Getenv("GC_ALIAS")
		if agentForQuery == "" {
			agentForQuery = os.Getenv("GC_SESSION_NAME")
		}
		if agentForQuery == "" {
			agentForQuery = os.Getenv("GC_AGENT")
		}
		sessionForQuery = os.Getenv("GC_SESSION_NAME")
	} else {
		sessionForQuery = cliSessionName(cityPath, cityName, resolvedAgentName, cfg.Workspace.SessionTemplate)
	}
	overrides, err := hookQueryEnv(cityPath, cfg, &a)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook: building work query env: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	overrides["GC_AGENT"] = agentForQuery
	overrides["GC_SESSION_NAME"] = sessionForQuery
	if sessionTemplateContext {
		overrides["GC_ALIAS"] = os.Getenv("GC_ALIAS")
		overrides["GC_SESSION_ID"] = os.Getenv("GC_SESSION_ID")
		overrides["GC_SESSION_ORIGIN"] = os.Getenv("GC_SESSION_ORIGIN")
		overrides["GC_TEMPLATE"] = os.Getenv("GC_TEMPLATE")
	}
	queryEnv := mergeRuntimeEnv(os.Environ(), overrides)
	runner := func(command, dir string) (string, error) {
		return shellWorkQueryWithEnv(command, dir, queryEnv)
	}

	// Lazy filter builder: drops in_progress beads claimed by another live
	// session so polecat B's hook doesn't re-acquire polecat A's bead (ga-8q6).
	// Built lazily — only when the work_query actually returns at least one
	// in_progress row with a non-empty assignee — so the common no-work path
	// avoids the extra session-bead scan and TestCmdHookSessionTemplate...
	// stays green. Self identifiers come out of the env / resolved names.
	buildFilter := func() liveAssigneeFilter {
		selfIdents := selfIdentifiers(resolvedAgentName, agentForQuery, sessionForQuery)
		return buildLiveAssigneeFilter(cityPath, selfIdents)
	}

	return doHookWithLiveFilter(workQuery, workDir, inject, runner, buildFilter, stdout, stderr)
}

// selfIdentifiers returns the set of strings that refer to the current
// session. Used to keep beads assigned to "this session" out of the
// double-dispatch filter — they're legitimately mine to work on. Sources:
// resolved agent name, work-query agent identity, work-query session
// name, plus GC_SESSION_ID / GC_SESSION_NAME / GC_ALIAS / GC_AGENT env.
func selfIdentifiers(parts ...string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, s := range parts {
		if v := strings.TrimSpace(s); v != "" {
			out[v] = struct{}{}
		}
	}
	for _, k := range []string{"GC_SESSION_ID", "GC_SESSION_NAME", "GC_ALIAS", "GC_AGENT"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			out[v] = struct{}{}
		}
	}
	return out
}

// hookQueryEnv returns the full work-query environment for a hook subprocess.
// It includes scope metadata (store root/scope/prefix) plus any rig-scoped
// runtime overrides so hook queries observe the same routing contract as the
// controller probes.
func hookQueryEnv(cityPath string, cfg *config.City, a *config.Agent) (map[string]string, error) {
	env, err := controllerWorkQueryEnv(cityPath, cfg, a)
	if err != nil {
		return nil, err
	}
	if env == nil {
		env = map[string]string{}
	}
	return env, nil
}

// WorkQueryRunner runs a work query command and returns its stdout.
// dir sets the command's working directory.
type WorkQueryRunner func(command, dir string) (string, error)

// hookWorkQueryTimeout caps the work-query subprocess. Default matches
// the pre-bounded behavior (30s) so existing tests that legitimately
// take >15s don't regress; the package-level var lets us lower it in
// follow-up work after slow paths are identified and optimized.
var hookWorkQueryTimeout = 30 * time.Second

// shellWorkQueryWithEnv runs a work query command via sh -c and returns
// stdout. If env is non-nil it is used as the subprocess environment
// (including any rig-scoped BEADS_DIR / GC_RIG_ROOT overrides); otherwise
// the child inherits the parent process environment. Times out after a
// short bounded interval so startup hooks cannot strand sessions behind a
// wedged data-plane command.
func shellWorkQueryWithEnv(command, dir string, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), hookWorkQueryTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.WaitDelay = 2 * time.Second
	prepareProviderOpCommand(cmd)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = workQueryEnvForDir(env, dir)
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return string(out), fmt.Errorf("running work query %q: timed out after %s with partial stdout %q", command, hookWorkQueryTimeout, msg)
		}
		return "", fmt.Errorf("running work query %q: timed out after %s", command, hookWorkQueryTimeout)
	}
	if err != nil {
		return "", fmt.Errorf("running work query %q: %w", command, err)
	}
	return string(out), nil
}

// workQueryEnvForDir ensures the subprocess environment does not carry a
// stale inherited PWD when exec.Cmd.Dir points somewhere else. Some shells
// (notably macOS /bin/sh) preserve the inherited PWD instead of recomputing
// it from the real working directory, which breaks hook work_query commands
// that inspect $PWD.
func workQueryEnvForDir(env []string, dir string) []string {
	if env == nil {
		env = mergeRuntimeEnv(os.Environ(), nil)
	}
	if dir == "" {
		return env
	}
	out := removeEnvKey(append([]string(nil), env...), "PWD")
	return append(out, "PWD="+dir)
}

// doHook is the pure logic for gc hook. Runs the work query and outputs
// results based on mode. Without inject: prints normalized ready-only output,
// returns 0 if work exists, 1 if empty. With inject: skips the work query and
// returns 0.
func doHook(workQuery, dir string, inject bool, runner WorkQueryRunner, stdout, stderr io.Writer) int {
	return doHookWithLiveFilter(workQuery, dir, inject, runner, nil, stdout, stderr)
}

// doHookWithLiveFilter is doHook plus a post-filter that drops in_progress
// beads claimed by another live polecat session. buildFilter is invoked
// only after the work_query returns at least one candidate row that the
// filter could actually drop (status=in_progress with a non-empty
// assignee) — that keeps the no-work-found path free of session-bead
// scanning. A nil builder disables the filter entirely (used by tests and
// by the bare doHook wrapper).
func doHookWithLiveFilter(workQuery, dir string, inject bool, runner WorkQueryRunner, buildFilter func() liveAssigneeFilter, stdout, stderr io.Writer) int {
	if inject {
		return 0
	}

	output, err := runner(workQuery, dir)
	if err != nil {
		if normalized := normalizeWorkQueryOutput(strings.TrimSpace(output)); normalized != "" {
			fmt.Fprint(stdout, normalized) //nolint:errcheck // best-effort stdout
		}
		fmt.Fprintf(stderr, "gc hook: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	trimmed := strings.TrimSpace(output)
	normalized := normalizeWorkQueryOutput(trimmed)
	normalized = filterUnreadyHookCandidates(normalized, time.Now())
	if buildFilter != nil && hasClaimedInProgressRow(normalized) {
		normalized = filterClaimedByLiveOthers(normalized, buildFilter())
	}
	hasWork := workQueryHasReadyWork(normalized)

	// Non-inject mode: print normalized, ready-only output. Return 0 only when work exists.
	if !hasWork {
		if normalized != "" {
			fmt.Fprint(stdout, normalized) //nolint:errcheck // best-effort stdout
		}
		return 1
	}
	fmt.Fprint(stdout, normalized) //nolint:errcheck // best-effort stdout
	return 0
}

// hasClaimedInProgressRow reports whether any row in the work_query
// output looks like a candidate for the live-assignee filter — i.e.
// status=in_progress with a non-empty assignee. Used to short-circuit
// the filter build (which scans session beads) when the work_query has
// returned nothing the filter could possibly drop.
func hasClaimedInProgressRow(output string) bool {
	if output == "" {
		return false
	}
	var decoded any
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		return false
	}
	arr, ok := decoded.([]any)
	if !ok {
		return false
	}
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		status, _ := obj["status"].(string)
		if !strings.EqualFold(strings.TrimSpace(status), "in_progress") {
			continue
		}
		assignee, _ := obj["assignee"].(string)
		if strings.TrimSpace(assignee) != "" {
			return true
		}
	}
	return false
}

// liveAssigneeFilter is the set of identifiers (session bead ID, runtime
// session name, alias, agent name) for live OTHER sessions — sessions in
// state active/awake/creating that are not the current hook caller. The
// zero value drops nothing; populated values cause filterClaimedByLiveOthers
// to drop in_progress rows whose assignee falls in the set.
//
// Rationale: routed work moves through the pool with
// gc.routed_to=<rig>/<template>, and bd ready returns the next eligible
// row. Without this filter, polecat B's work_query fired while polecat A
// already holds an in_progress bead re-acquires the same row and
// double-dispatches the work (see ga-8q6). When the previous holder is
// already drained/closed, the bead is rescue-able and kept.
type liveAssigneeFilter struct {
	liveOtherAssignees map[string]struct{}
}

func (f liveAssigneeFilter) shouldDrop(item map[string]any) bool {
	if len(f.liveOtherAssignees) == 0 {
		return false
	}
	status, _ := item["status"].(string)
	if !strings.EqualFold(strings.TrimSpace(status), "in_progress") {
		return false
	}
	assignee, _ := item["assignee"].(string)
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return false
	}
	_, live := f.liveOtherAssignees[assignee]
	return live
}

// filterClaimedByLiveOthers re-encodes output with in_progress rows
// dropped when their assignee belongs to a live OTHER session. Non-JSON
// and non-array outputs are returned unchanged so the function composes
// safely with the rest of the normalize/filter chain.
func filterClaimedByLiveOthers(output string, filter liveAssigneeFilter) string {
	if output == "" || len(filter.liveOtherAssignees) == 0 {
		return output
	}
	var decoded any
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		return output
	}
	arr, ok := decoded.([]any)
	if !ok {
		return output
	}
	filtered := make([]any, 0, len(arr))
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			filtered = append(filtered, item)
			continue
		}
		if filter.shouldDrop(obj) {
			continue
		}
		filtered = append(filtered, obj)
	}
	reencoded, err := json.Marshal(filtered)
	if err != nil {
		return output
	}
	return string(reencoded)
}

// buildLiveAssigneeFilter opens the city store and returns a filter
// listing all live OTHER sessions' identifiers. Sessions in state
// active/awake/creating contribute their bead ID, runtime session name,
// alias, and agent name; any of those that also appears in selfIdents
// is skipped so the current session's own claimed beads pass through.
// On any store-access error the returned filter is empty so hooks keep
// working even when session metadata is unavailable.
func buildLiveAssigneeFilter(cityPath string, selfIdents map[string]struct{}) liveAssigneeFilter {
	if cityPath == "" {
		return liveAssigneeFilter{}
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil || store == nil {
		return liveAssigneeFilter{}
	}
	sessionBeads, err := session.ListAllSessionBeads(store, beads.ListQuery{})
	if err != nil && !beads.IsPartialResult(err) {
		return liveAssigneeFilter{}
	}
	live := map[string]struct{}{}
	for _, b := range sessionBeads {
		if b.Status == "closed" {
			continue
		}
		if !isLiveSessionState(b.Metadata["state"]) {
			continue
		}
		for _, id := range []string{
			b.ID,
			b.Metadata["session_name"],
			b.Metadata["alias"],
			b.Metadata["agent_name"],
		} {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if _, isSelf := selfIdents[id]; isSelf {
				continue
			}
			live[id] = struct{}{}
		}
	}
	return liveAssigneeFilter{liveOtherAssignees: live}
}

func isLiveSessionState(raw string) bool {
	switch strings.TrimSpace(raw) {
	case "active", "awake", "creating":
		return true
	default:
		return false
	}
}

func workQueryHasReadyWork(output string) bool {
	if output == "" {
		return false
	}
	// Newer bd versions print a human-readable no-work line to stdout instead
	// of staying silent. Treat that as "no work" for hooks and WakeWork.
	if strings.Contains(output, "No ready work found") {
		return false
	}
	var decoded any
	if err := json.Unmarshal([]byte(output), &decoded); err == nil {
		switch v := decoded.(type) {
		case []any:
			return len(v) > 0
		case map[string]any:
			return len(v) > 0
		case nil:
			return false
		}
	}
	return true
}

// filterUnreadyHookCandidates strips beads from work_query output that fail
// bd ready semantics: a message issue_type, a closed primary status, future
// defer_until, or any open blocking dep in the row's blocked_by array. The
// work_query is expected to gate these, but defensive filtering here prevents
// a single broken query from cascading into agent action on a bead it cannot
// progress. The closed-status guard specifically defends against Dolt
// status secondary-index drift, where a stale index can make
// `bd list --status=open` return a row whose primary status is closed. The
// message guard closes the Tier-1 gap where `bd list --status in_progress
// --assignee` (which skips bd ready-logic) could surface a mail bead
// addressed to the session as work (ga-om6b, upstream #4419 #4442).
// Pure function over JSON; takes time.Time so tests stay deterministic.
func filterUnreadyHookCandidates(output string, now time.Time) string {
	if output == "" {
		return output
	}
	var decoded any
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		return output
	}
	arr, ok := decoded.([]any)
	if !ok {
		return output
	}
	filtered := make([]any, 0, len(arr))
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			filtered = append(filtered, item)
			continue
		}
		if isMessageHookCandidate(obj) {
			continue
		}
		if isClosedHookCandidate(obj) {
			continue
		}
		if isFutureDeferredHookCandidate(obj, now) {
			continue
		}
		if isDepBlockedHookCandidate(obj) {
			continue
		}
		filtered = append(filtered, obj)
	}
	reencoded, err := json.Marshal(filtered)
	if err != nil {
		return output
	}
	return string(reencoded)
}

// isMessageHookCandidate reports whether a work_query row is a mail bead
// (issue_type == "message", case-insensitive, trimmed). Mail sent to a session
// sets Assignee = recipient (see internal/mail/beadmail), so a message bead can
// otherwise be returned as work — most visibly by the Tier-1
// `bd list --status in_progress --assignee` query, which does not pass through
// bd ready-logic (the only tier that already omits messages). Dropping message
// beads here keeps a polecat from parking on a mail bead instead of real routed
// work (ga-om6b, upstream #4419 #4442).
func isMessageHookCandidate(item map[string]any) bool {
	issueType, ok := item["issue_type"].(string)
	return ok && strings.EqualFold(strings.TrimSpace(issueType), "message")
}

// isClosedHookCandidate reports whether a work_query row carries a primary
// status of "closed" (case-insensitive, trimmed). Dolt status secondary-index
// drift can make a `--status=open` query surface a row whose primary row is
// already closed; dispatching such a row acts on completed work. This guard
// drops it defensively.
func isClosedHookCandidate(item map[string]any) bool {
	status, ok := item["status"].(string)
	return ok && strings.EqualFold(strings.TrimSpace(status), "closed")
}

func isFutureDeferredHookCandidate(item map[string]any, now time.Time) bool {
	raw, ok := item["defer_until"].(string)
	if !ok {
		return false
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	deferAt, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return false
	}
	return deferAt.After(now)
}

func isDepBlockedHookCandidate(item map[string]any) bool {
	blockedBy, ok := item["blocked_by"].([]any)
	if !ok || len(blockedBy) == 0 {
		return false
	}
	for _, b := range blockedBy {
		dep, ok := b.(map[string]any)
		if !ok {
			continue
		}
		status, ok := dep["status"].(string)
		if !ok {
			continue
		}
		status = strings.TrimSpace(status)
		if status != "" && !strings.EqualFold(status, "closed") {
			return true
		}
	}
	return false
}

func normalizeWorkQueryOutput(output string) string {
	if output == "" {
		return output
	}
	var decoded any
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		return output
	}
	if _, ok := decoded.(map[string]any); !ok {
		return output
	}
	normalized, err := json.Marshal([]any{decoded})
	if err != nil {
		return output
	}
	return string(normalized)
}
