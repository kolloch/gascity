package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestFilterClaimedByLiveOthersDropsOtherLiveAssignee covers the headline
// fix: a routed-pool row in status=in_progress with an assignee that
// matches another live polecat session must not surface in the hook
// output. Without the filter, the bead is returned to multiple polecats
// in the same pool and dispatched twice (ga-8q6).
func TestFilterClaimedByLiveOthersDropsOtherLiveAssignee(t *testing.T) {
	filter := liveAssigneeFilter{liveOtherAssignees: map[string]struct{}{
		"gastown__polecat-pe-other": {},
	}}
	input := `[{"id":"hw-1","status":"in_progress","assignee":"gastown__polecat-pe-other"}]`

	got := filterClaimedByLiveOthers(input, filter)

	if strings.Contains(got, "hw-1") {
		t.Fatalf("filterClaimedByLiveOthers kept bead claimed by live other: %s", got)
	}
}

// TestFilterClaimedByLiveOthersKeepsDeadAssignee guards the rescue path:
// when a polecat dies mid-work, its bead must remain visible to the next
// polecat so the work can be resumed. Only LIVE other assignees suppress
// the row.
func TestFilterClaimedByLiveOthersKeepsDeadAssignee(t *testing.T) {
	// Filter contains only the LIVE session; the bead's assignee is a
	// different (dead) session, so the bead is rescue-able.
	filter := liveAssigneeFilter{liveOtherAssignees: map[string]struct{}{
		"gastown__polecat-pe-other": {},
	}}
	input := `[{"id":"hw-1","status":"in_progress","assignee":"gastown__polecat-pe-dead"}]`

	got := filterClaimedByLiveOthers(input, filter)

	if !strings.Contains(got, "hw-1") {
		t.Fatalf("filterClaimedByLiveOthers dropped rescue-able bead: %s", got)
	}
}

// TestFilterClaimedByLiveOthersKeepsSelfAssignee ensures the current
// session can still see its own in-progress beads — the build helper is
// responsible for excluding self identifiers from the live-other set,
// but the filter must also tolerate a self-assignee that accidentally
// lands in the set.
func TestFilterClaimedByLiveOthersKeepsSelfAssignee(t *testing.T) {
	// Empty live set — the build path excludes self before the filter
	// runs. With no live-other assignees, no row is dropped.
	filter := liveAssigneeFilter{liveOtherAssignees: map[string]struct{}{}}
	input := `[{"id":"hw-1","status":"in_progress","assignee":"gastown__polecat-pe-me"}]`

	got := filterClaimedByLiveOthers(input, filter)

	if !strings.Contains(got, "hw-1") {
		t.Fatalf("filterClaimedByLiveOthers dropped self-assigned bead: %s", got)
	}
}

// TestFilterClaimedByLiveOthersKeepsOpenStatus covers the status guard:
// only in_progress rows are dropped. An open row assigned to a live
// session is unusual but still keeps moving — the pool consumer will
// have to claim it before working.
func TestFilterClaimedByLiveOthersKeepsOpenStatus(t *testing.T) {
	filter := liveAssigneeFilter{liveOtherAssignees: map[string]struct{}{
		"gastown__polecat-pe-other": {},
	}}
	input := `[{"id":"hw-1","status":"open","assignee":"gastown__polecat-pe-other"}]`

	got := filterClaimedByLiveOthers(input, filter)

	if !strings.Contains(got, "hw-1") {
		t.Fatalf("filterClaimedByLiveOthers dropped open bead: %s", got)
	}
}

// TestFilterClaimedByLiveOthersKeepsEmptyAssignee covers the unassigned
// (pool) case — `bd ready --unassigned` returns rows with no assignee at
// all, and those must pass through.
func TestFilterClaimedByLiveOthersKeepsEmptyAssignee(t *testing.T) {
	filter := liveAssigneeFilter{liveOtherAssignees: map[string]struct{}{
		"gastown__polecat-pe-other": {},
	}}
	input := `[{"id":"hw-1","status":"in_progress","assignee":""}]`

	got := filterClaimedByLiveOthers(input, filter)

	if !strings.Contains(got, "hw-1") {
		t.Fatalf("filterClaimedByLiveOthers dropped unassigned bead: %s", got)
	}
}

// TestFilterClaimedByLiveOthersZeroValueIsNoOp ensures the filter is
// strictly opt-in: callers that don't populate liveOtherAssignees see
// identical output. Critical for doHook backward compatibility — the
// non-filter wrapper passes a zero value and must not affect anything.
func TestFilterClaimedByLiveOthersZeroValueIsNoOp(t *testing.T) {
	input := `[{"id":"hw-1","status":"in_progress","assignee":"gastown__polecat-pe-other"}]`

	got := filterClaimedByLiveOthers(input, liveAssigneeFilter{})

	if got != input {
		t.Fatalf("zero-value filter modified output: got=%s want=%s", got, input)
	}
}

// TestFilterClaimedByLiveOthersPreservesNonJSON guards the
// normalize/filter chain — `filterUnreadyHookCandidates` already
// short-circuits on non-JSON input, and the new filter must do the
// same so a single broken work_query output doesn't blank the hook.
func TestFilterClaimedByLiveOthersPreservesNonJSON(t *testing.T) {
	filter := liveAssigneeFilter{liveOtherAssignees: map[string]struct{}{
		"gastown__polecat-pe-other": {},
	}}
	input := "✨ No ready work found\n"

	got := filterClaimedByLiveOthers(input, filter)

	if got != input {
		t.Fatalf("non-JSON input rewritten: got=%q want=%q", got, input)
	}
}

// TestFilterClaimedByLiveOthersEmptyOutput covers the empty-input edge
// — the work_query produces no output when there is no work, and the
// filter must not error or rewrite.
func TestFilterClaimedByLiveOthersEmptyOutput(t *testing.T) {
	filter := liveAssigneeFilter{liveOtherAssignees: map[string]struct{}{
		"gastown__polecat-pe-other": {},
	}}

	got := filterClaimedByLiveOthers("", filter)

	if got != "" {
		t.Fatalf("empty input rewritten: %q", got)
	}
}

// TestFilterClaimedByLiveOthersKeepsRowsWithoutAssigneeField ensures
// the filter tolerates work_query outputs that omit the "assignee"
// JSON field altogether (the field is optional in bd's schema).
func TestFilterClaimedByLiveOthersKeepsRowsWithoutAssigneeField(t *testing.T) {
	filter := liveAssigneeFilter{liveOtherAssignees: map[string]struct{}{
		"gastown__polecat-pe-other": {},
	}}
	input := `[{"id":"hw-1","status":"open"}]`

	got := filterClaimedByLiveOthers(input, filter)

	if !strings.Contains(got, "hw-1") {
		t.Fatalf("filterClaimedByLiveOthers dropped row without assignee field: %s", got)
	}
}

// TestFilterClaimedByLiveOthersDropsAllInArray exercises the stress
// scenario from the acceptance criteria — when every row in the output
// is claimed by live others, the result is an empty array, not an
// error. workQueryHasReadyWork then returns false and the hook reports
// no work, which is the correct outcome for polecat B.
func TestFilterClaimedByLiveOthersDropsAllInArray(t *testing.T) {
	filter := liveAssigneeFilter{liveOtherAssignees: map[string]struct{}{
		"gastown__polecat-pe-a": {},
		"gastown__polecat-pe-b": {},
	}}
	input := `[
		{"id":"hw-1","status":"in_progress","assignee":"gastown__polecat-pe-a"},
		{"id":"hw-2","status":"in_progress","assignee":"gastown__polecat-pe-b"}
	]`

	got := filterClaimedByLiveOthers(input, filter)

	// Result must be a valid JSON array. We don't pin to "[]" exactly
	// because json.Marshal could produce "null" for nil slices — but
	// the buildup uses make([]any, 0, ...) so it stays as "[]".
	if strings.Contains(got, "hw-1") || strings.Contains(got, "hw-2") {
		t.Fatalf("filterClaimedByLiveOthers kept claimed beads: %s", got)
	}
	if strings.TrimSpace(got) != "[]" {
		t.Fatalf("expected empty array, got %q", got)
	}
}

// TestDoHookFiltersClaimedByLiveOthersEndToEnd is the integration-style
// test that pipes a work_query producing a claimed bead through the
// full doHookWithLiveFilter path. With the live filter populated for
// the other session, the bead is suppressed and the hook reports no
// work (exit 1).
func TestDoHookFiltersClaimedByLiveOthersEndToEnd(t *testing.T) {
	runner := func(_, _ string) (string, error) {
		return `[{"id":"hw-1","status":"in_progress","assignee":"gastown__polecat-pe-other"}]`, nil
	}
	build := func() liveAssigneeFilter {
		return liveAssigneeFilter{liveOtherAssignees: map[string]struct{}{
			"gastown__polecat-pe-other": {},
		}}
	}

	var stdout, stderr bytes.Buffer
	code := doHookWithLiveFilter("bd ready", ".", false, runner, build, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("doHookWithLiveFilter() = %d, want 1 (no work after filter); stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "hw-1") {
		t.Fatalf("stdout still contains filtered bead: %s", stdout.String())
	}
}

// TestDoHookKeepsRescueWorkWhenAssigneeIsDead is the rescue-path
// counterpart: same bead structure as above, but the assignee is NOT in
// the live set, so the bead surfaces normally for resumption.
func TestDoHookKeepsRescueWorkWhenAssigneeIsDead(t *testing.T) {
	runner := func(_, _ string) (string, error) {
		return `[{"id":"hw-1","status":"in_progress","assignee":"gastown__polecat-pe-dead"}]`, nil
	}
	// Only a different session is live; the dead one isn't.
	build := func() liveAssigneeFilter {
		return liveAssigneeFilter{liveOtherAssignees: map[string]struct{}{
			"gastown__polecat-pe-other": {},
		}}
	}

	var stdout, stderr bytes.Buffer
	code := doHookWithLiveFilter("bd ready", ".", false, runner, build, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("doHookWithLiveFilter() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "hw-1") {
		t.Fatalf("rescue-able bead missing from stdout: %s", stdout.String())
	}
}

// TestDoHookSkipsFilterBuildWhenOutputHasNoClaimedRows guards the lazy
// builder contract: when the work_query returned no in_progress+assigned
// rows, the filter builder must never be invoked. This is what keeps
// the no-work path free of the session-bead scan, satisfying
// TestCmdHookSessionTemplateContextDoesNotScanSessionsForName.
func TestDoHookSkipsFilterBuildWhenOutputHasNoClaimedRows(t *testing.T) {
	for name, work := range map[string]string{
		"empty array":                  `[]`,
		"empty output":                 ``,
		"open only":                    `[{"id":"hw-1","status":"open"}]`,
		"in_progress without assignee": `[{"id":"hw-1","status":"in_progress","assignee":""}]`,
	} {
		t.Run(name, func(t *testing.T) {
			runner := func(_, _ string) (string, error) {
				return work, nil
			}
			called := false
			build := func() liveAssigneeFilter {
				called = true
				return liveAssigneeFilter{}
			}

			var stdout, stderr bytes.Buffer
			_ = doHookWithLiveFilter("bd ready", ".", false, runner, build, &stdout, &stderr)

			if called {
				t.Fatalf("filter builder invoked for non-candidate output %q", work)
			}
		})
	}
}

// TestDoHookInvokesFilterBuildOnceForClaimedRow confirms the lazy
// builder is invoked when at least one row is a filter candidate.
// Pairing with the test above gives both directions of the contract.
func TestDoHookInvokesFilterBuildOnceForClaimedRow(t *testing.T) {
	runner := func(_, _ string) (string, error) {
		return `[{"id":"hw-1","status":"in_progress","assignee":"gastown__polecat-pe-other"}]`, nil
	}
	calls := 0
	build := func() liveAssigneeFilter {
		calls++
		return liveAssigneeFilter{liveOtherAssignees: map[string]struct{}{
			"gastown__polecat-pe-other": {},
		}}
	}

	var stdout, stderr bytes.Buffer
	_ = doHookWithLiveFilter("bd ready", ".", false, runner, build, &stdout, &stderr)

	if calls != 1 {
		t.Fatalf("build invoked %d times, want exactly 1", calls)
	}
}

// TestHasClaimedInProgressRowDetectsCandidates covers the predicate
// that decides whether the lazy builder runs. False positives waste a
// session-bead scan; false negatives let claimed beads slip through.
func TestHasClaimedInProgressRowDetectsCandidates(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"empty array", `[]`, false},
		{"open status", `[{"id":"x","status":"open","assignee":"y"}]`, false},
		{"in_progress no assignee", `[{"id":"x","status":"in_progress"}]`, false},
		{"in_progress empty assignee", `[{"id":"x","status":"in_progress","assignee":""}]`, false},
		{"in_progress with assignee", `[{"id":"x","status":"in_progress","assignee":"sess-1"}]`, true},
		{"mixed", `[{"id":"x","status":"open"},{"id":"y","status":"in_progress","assignee":"sess-1"}]`, true},
		{"non-array", `{"id":"x","status":"in_progress","assignee":"sess-1"}`, false},
		{"non-JSON", `Hello`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasClaimedInProgressRow(tc.in); got != tc.want {
				t.Errorf("hasClaimedInProgressRow(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestIsLiveSessionState pins the live-state set to active/awake/creating.
// Any other state (suspended, draining, drained, archived, closed,
// failed-create, quarantined, asleep, "") is treated as not live, so the
// session's claimed beads remain rescue-able by another polecat.
func TestIsLiveSessionState(t *testing.T) {
	for _, tc := range []struct {
		state string
		want  bool
	}{
		{"active", true},
		{"awake", true},
		{"creating", true},
		{" active ", true}, // tolerate whitespace
		{"asleep", false},
		{"suspended", false},
		{"draining", false},
		{"drained", false},
		{"archived", false},
		{"failed-create", false},
		{"quarantined", false},
		{"", false},
		{"closed", false},
	} {
		if got := isLiveSessionState(tc.state); got != tc.want {
			t.Errorf("isLiveSessionState(%q) = %v, want %v", tc.state, got, tc.want)
		}
	}
}

// TestBuildLiveAssigneeFilterAgainstSessionStore opens a real bead
// store containing a mix of live, dead, and self session beads and
// confirms the filter picks only the live OTHER sessions' identifiers
// — bead ID, session name, alias, agent name. This is the contract the
// acceptance criterion at the bead-store level relies on. Uses the
// file-backed store so the test doesn't spawn a dolt sql-server.
func TestBuildLiveAssigneeFilterAgainstSessionStore(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}

	mustCreate := func(state string, metadata map[string]string) beads.Bead {
		t.Helper()
		md := map[string]string{"state": state}
		for k, v := range metadata {
			md[k] = v
		}
		got, err := store.Create(beads.Bead{
			Title:    "session bead",
			Type:     session.BeadType,
			Status:   "open",
			Labels:   []string{session.LabelSession},
			Metadata: md,
		})
		if err != nil {
			t.Fatalf("Create(%v): %v", metadata, err)
		}
		return got
	}

	// Live OTHER polecat — its identifiers should be in the filter.
	other := mustCreate("active", map[string]string{
		"session_name": "gastown__polecat-pe-other",
		"agent_name":   "gastown__polecat-pe-other",
	})
	// Live OTHER witness — also gets filtered (any live session, not just polecats).
	witness := mustCreate("awake", map[string]string{
		"session_name": "gastown__witness-pe-witness",
		"alias":        "gastown/gastown.witness",
		"agent_name":   "gastown/gastown.witness",
	})
	// Dead polecat — its identifiers should NOT be in the filter
	// (rescue path).
	dead := mustCreate("drained", map[string]string{
		"session_name": "gastown__polecat-pe-dead",
	})
	// Suspended polecat — same, not in the filter.
	suspended := mustCreate("suspended", map[string]string{
		"session_name": "gastown__polecat-pe-suspended",
	})
	// SELF (current session) — must be excluded even though live.
	self := mustCreate("active", map[string]string{
		"session_name": "gastown__polecat-pe-me",
	})

	selfIdents := map[string]struct{}{
		self.ID:                  {},
		"gastown__polecat-pe-me": {},
	}
	filter := buildLiveAssigneeFilter(cityDir, selfIdents)

	wantLive := []string{
		other.ID,
		"gastown__polecat-pe-other",
		witness.ID,
		"gastown__witness-pe-witness",
		"gastown/gastown.witness",
	}
	for _, id := range wantLive {
		if _, ok := filter.liveOtherAssignees[id]; !ok {
			t.Errorf("expected live identifier %q in filter, got %v", id, sortedKeys(filter.liveOtherAssignees))
		}
	}
	wantAbsent := []string{
		dead.ID,
		"gastown__polecat-pe-dead",
		suspended.ID,
		"gastown__polecat-pe-suspended",
		self.ID,
		"gastown__polecat-pe-me",
	}
	for _, id := range wantAbsent {
		if _, ok := filter.liveOtherAssignees[id]; ok {
			t.Errorf("expected identifier %q absent from filter, got %v", id, sortedKeys(filter.liveOtherAssignees))
		}
	}
}

// TestBuildLiveAssigneeFilterReturnsEmptyOnStoreError is the
// best-effort guard: if openCityStoreAt fails, the filter must be empty
// (a no-op) so hooks keep working when session metadata is unreachable.
func TestBuildLiveAssigneeFilterReturnsEmptyOnStoreError(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")

	// Point at a path that's not a city dir — openCityStoreAt will fail.
	filter := buildLiveAssigneeFilter("/nonexistent/city/path", nil)

	if len(filter.liveOtherAssignees) != 0 {
		t.Fatalf("filter on bad path = %v, want empty", filter.liveOtherAssignees)
	}
}

// TestBuildLiveAssigneeFilterEmptyPath is the explicit zero-input case:
// no city path, no work. The wrapper in cmdHookWithFormat could pass
// cityPath="" if resolution silently failed; the filter must tolerate it.
func TestBuildLiveAssigneeFilterEmptyPath(t *testing.T) {
	filter := buildLiveAssigneeFilter("", nil)
	if len(filter.liveOtherAssignees) != 0 {
		t.Fatalf("filter on empty path = %v, want empty", filter.liveOtherAssignees)
	}
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// stable order for diagnostic output
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// jsonObj is a tiny helper for assembling the JSON that work_query
// outputs in tests. Keeps the test bodies focused on the filter logic
// instead of escaping minutiae.
func jsonObj(t *testing.T, fields map[string]any) string {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("json.Marshal(%v): %v", fields, err)
	}
	return string(b)
}

// TestCmdHookFiltersBeadClaimedByLiveOtherPolecat is the spec
// acceptance test from ga-8q6:
//
//	Repro: route a bead, claim it on polecat A, then run gc hook on
//	polecat B in the same pool. Polecat B sees nothing (or the next
//	eligible bead, not the one A holds).
//
// This drives cmdHookWithFormat end-to-end: a real file-backed city
// store with a live "other" polecat session, a fake bd binary that
// returns the bead polecat A holds, and the expectation that
// cmdHookWithFormat exits 1 with no work surfaced.
func TestCmdHookFiltersBeadClaimedByLiveOtherPolecat(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	fakeBin := t.TempDir()

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "worker"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Plant a live "other" polecat session bead in the city store so
	// buildLiveAssigneeFilter sees it.
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "other live polecat",
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":        "active",
			"session_name": "gastown__polecat-pe-other",
			"agent_name":   "gastown__polecat-pe-other",
		},
	}); err != nil {
		t.Fatalf("Create other session bead: %v", err)
	}

	// Fake bd: return the bead the other polecat is already working.
	fakeBD := filepath.Join(fakeBin, "bd")
	script := "#!/bin/sh\nprintf '[{\"id\":\"hw-1\",\"status\":\"in_progress\",\"assignee\":\"gastown__polecat-pe-other\"}]'\n"
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_TMUX_SESSION", "polecat-b")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithFormat([]string{"worker"}, false, "", &stdout, &stderr)

	if code != 1 {
		t.Fatalf("cmdHookWithFormat() = %d, want 1 (no work after filter); stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "hw-1") {
		t.Fatalf("stdout still contains filtered bead: %s", stdout.String())
	}
}

// TestCmdHookSurfacesBeadWhenAssigneeIsSelf complements the spec test
// by confirming the filter does not strip away beads belonging to the
// current session. Without this, a polecat that crashed mid-task and
// came back up via the same session name would never see its own
// in_progress work.
func TestCmdHookSurfacesBeadWhenAssigneeIsSelf(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	fakeBin := t.TempDir()

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "worker"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	// Plant a live session bead whose session_name matches our
	// GC_SESSION_NAME below — it represents "this session".
	if _, err := store.Create(beads.Bead{
		Title:  "self session",
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":        "active",
			"session_name": "polecat-self",
			"agent_name":   "polecat-self",
		},
	}); err != nil {
		t.Fatalf("Create self session bead: %v", err)
	}

	fakeBD := filepath.Join(fakeBin, "bd")
	script := "#!/bin/sh\nprintf '[{\"id\":\"hw-1\",\"status\":\"in_progress\",\"assignee\":\"polecat-self\"}]'\n"
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_TMUX_SESSION", "polecat-self")
	t.Setenv("GC_SESSION_NAME", "polecat-self")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithFormat([]string{"worker"}, false, "", &stdout, &stderr)

	if code != 0 {
		t.Fatalf("cmdHookWithFormat() = %d, want 0 (self-assigned bead must surface); stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "hw-1") {
		t.Fatalf("self-assigned bead missing from stdout: %s", stdout.String())
	}
}

// TestFilterClaimedByLiveOthersMatchesAcrossIdentityForms documents
// that an assignee can take multiple equivalent forms — session name,
// bead ID, alias, agent_name — and the filter recognizes each as long
// as the build helper folded the form into the live-other set.
func TestFilterClaimedByLiveOthersMatchesAcrossIdentityForms(t *testing.T) {
	filter := liveAssigneeFilter{liveOtherAssignees: map[string]struct{}{
		"pe-other":                  {},
		"gastown__polecat-pe-other": {},
		"polecat-other-alias":       {},
	}}

	for _, assignee := range []string{
		"pe-other",
		"gastown__polecat-pe-other",
		"polecat-other-alias",
	} {
		row := jsonObj(t, map[string]any{
			"id":       "hw-1",
			"status":   "in_progress",
			"assignee": assignee,
		})
		input := fmt.Sprintf("[%s]", row)
		got := filterClaimedByLiveOthers(input, filter)
		if strings.Contains(got, "hw-1") {
			t.Errorf("assignee form %q: bead not filtered; got=%s", assignee, got)
		}
	}
}
