package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestDoHookFiltersClaimedByOtherSession reproduces pe-wisp-8ocps2 / di-tz1:
// the bd ready query has surfaced a bead that another live session has
// already claimed. Without the post-filter, two pool members race to act
// on the same work.
func TestDoHookFiltersClaimedByOtherSession(t *testing.T) {
	t.Setenv("GC_SESSION_NAME", "gastown__polecat-pe-uvdiy")
	t.Setenv("GC_SESSION_ID", "pe-uvdiy")
	t.Setenv("GC_ALIAS", "")

	runner := func(_, _ string) (string, error) {
		return `[
			{"id":"za-ukbz","status":"open","assignee":"gastown__polecat-pe-ow4p4","metadata":{"gc.routed_to":"dipcity/gastown.polecat"}},
			{"id":"clear-1","status":"open"}
		]`, nil
	}

	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", ".", false, runner, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "za-ukbz") {
		t.Errorf("bead claimed by another session surfaced in hook output: %s", out)
	}
	if !strings.Contains(out, "clear-1") {
		t.Errorf("unclaimed bead missing from hook output: %s", out)
	}
}

// TestDoHookKeepsBeadClaimedByMe ensures Tier 1/2 crash-recovery surfacing
// still works: a bead assigned to one of the caller's identities is the
// caller's own work and must pass through the filter unchanged.
func TestDoHookKeepsBeadClaimedByMe(t *testing.T) {
	t.Setenv("GC_SESSION_NAME", "gastown__polecat-pe-uvdiy")
	t.Setenv("GC_SESSION_ID", "pe-uvdiy")
	t.Setenv("GC_ALIAS", "")

	runner := func(_, _ string) (string, error) {
		return `[
			{"id":"mine-by-name","status":"in_progress","assignee":"gastown__polecat-pe-uvdiy","metadata":{"gc.routed_to":"dipcity/gastown.polecat"}},
			{"id":"mine-by-id","status":"in_progress","assignee":"pe-uvdiy","metadata":{"gc.routed_to":"dipcity/gastown.polecat"}}
		]`, nil
	}

	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", ".", false, runner, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"mine-by-name", "mine-by-id"} {
		if !strings.Contains(out, want) {
			t.Errorf("own claim %q stripped by filter: %s", want, out)
		}
	}
}

// TestDoHookKeepsBeadClaimedByMyAlias covers the named-always case
// (refinery, witness, etc.) where the agent identity is GC_ALIAS rather
// than an ephemeral session ID.
func TestDoHookKeepsBeadClaimedByMyAlias(t *testing.T) {
	t.Setenv("GC_SESSION_NAME", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_ALIAS", "dipcity/gastown.refinery")

	runner := func(_, _ string) (string, error) {
		return `[
			{"id":"refinery-work","status":"open","assignee":"dipcity/gastown.refinery","metadata":{"gc.routed_to":"dipcity/gastown.refinery"}}
		]`, nil
	}

	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", ".", false, runner, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "refinery-work") {
		t.Errorf("own claim (alias) filtered out: %s", stdout.String())
	}
}

// TestDoHookKeepsPoolPlaceholderAssignee guards the Tier 3b pattern (ga-2pz):
// some callers park work on the pool template name as a placeholder. The
// filter must keep such beads since they are functionally unclaimed.
func TestDoHookKeepsPoolPlaceholderAssignee(t *testing.T) {
	t.Setenv("GC_SESSION_NAME", "gastown__polecat-pe-uvdiy")
	t.Setenv("GC_SESSION_ID", "pe-uvdiy")
	t.Setenv("GC_ALIAS", "")

	runner := func(_, _ string) (string, error) {
		return `[
			{"id":"parked","status":"open","assignee":"dipcity/gastown.polecat","metadata":{"gc.routed_to":"dipcity/gastown.polecat"}}
		]`, nil
	}

	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", ".", false, runner, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "parked") {
		t.Errorf("pool-placeholder bead filtered out: %s", stdout.String())
	}
}

func TestDoHookEmptyAssigneeKept(t *testing.T) {
	t.Setenv("GC_SESSION_NAME", "gastown__polecat-pe-uvdiy")
	t.Setenv("GC_SESSION_ID", "pe-uvdiy")
	t.Setenv("GC_ALIAS", "")

	runner := func(_, _ string) (string, error) {
		return `[
			{"id":"empty-assignee","status":"open","assignee":""},
			{"id":"no-assignee","status":"open"}
		]`, nil
	}

	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", ".", false, runner, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"empty-assignee", "no-assignee"} {
		if !strings.Contains(out, want) {
			t.Errorf("bead %q with no assignee was filtered: %s", want, out)
		}
	}
}

// TestDoHookNoIdentitiesStripsForeignClaim covers the controller-probe
// path: when run without any session identity (GC_SESSION_* all empty),
// foreign claims are still recognized and stripped. The probe should not
// see "demand" for a bead another session is already executing.
func TestDoHookNoIdentitiesStripsForeignClaim(t *testing.T) {
	t.Setenv("GC_SESSION_NAME", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_ALIAS", "")

	runner := func(_, _ string) (string, error) {
		return `[
			{"id":"foreign","status":"open","assignee":"gastown__polecat-pe-other","metadata":{"gc.routed_to":"dipcity/gastown.polecat"}},
			{"id":"unassigned","status":"open"}
		]`, nil
	}

	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", ".", false, runner, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "foreign") {
		t.Errorf("foreign-assigned bead surfaced when no identities set: %s", out)
	}
	if !strings.Contains(out, "unassigned") {
		t.Errorf("unassigned bead missing from output: %s", out)
	}
}

// Pure-filter unit tests mirror the pattern set by filterUnreadyHookCandidates:
// deterministic input, no env reads, parameterized identities.

func TestFilterClaimedByOtherSession_StripsForeign(t *testing.T) {
	in := `[
		{"id":"foreign","assignee":"other-session","metadata":{"gc.routed_to":"pool"}},
		{"id":"blank","assignee":""},
		{"id":"mine","assignee":"me"}
	]`
	out := filterClaimedByOtherSession(in, []string{"me"})
	if strings.Contains(out, `"foreign"`) {
		t.Errorf("did not strip foreign-claim row: %s", out)
	}
	for _, want := range []string{`"blank"`, `"mine"`} {
		if !strings.Contains(out, want) {
			t.Errorf("stripped unintended row %q: %s", want, out)
		}
	}
}

func TestFilterClaimedByOtherSession_KeepsPoolPlaceholder(t *testing.T) {
	in := `[{"id":"parked","assignee":"pool","metadata":{"gc.routed_to":"pool"}}]`
	out := filterClaimedByOtherSession(in, []string{"me"})
	if !strings.Contains(out, `"parked"`) {
		t.Errorf("stripped pool-placeholder bead: %s", out)
	}
}

func TestFilterClaimedByOtherSession_PreservesMalformedJSON(t *testing.T) {
	// Match filterUnreadyHookCandidates: malformed JSON and single-object
	// JSON are passed through unchanged so callers can decide.
	cases := []string{
		"not json",
		`{"id":"single-obj"}`,
		"",
	}
	for _, c := range cases {
		if got := filterClaimedByOtherSession(c, []string{"me"}); got != c {
			t.Errorf("filter mangled non-array input %q -> %q", c, got)
		}
	}
}

func TestFilterClaimedByOtherSession_BlankIdentitiesIgnored(t *testing.T) {
	in := `[{"id":"mine","assignee":"me"}]`
	out := filterClaimedByOtherSession(in, []string{"  ", "", "me"})
	if !strings.Contains(out, `"mine"`) {
		t.Errorf("blank identities should be ignored, not block own claim: %s", out)
	}
}

func TestFilterClaimedByOtherSession_NoMetadataDropsForeign(t *testing.T) {
	// If the bead has no metadata field at all, a foreign assignee is still
	// a foreign claim — we have no evidence it's the pool placeholder.
	in := `[{"id":"orphan","assignee":"other-session"}]`
	out := filterClaimedByOtherSession(in, []string{"me"})
	if strings.Contains(out, `"orphan"`) {
		t.Errorf("expected foreign-claim with no metadata to be stripped: %s", out)
	}
}
