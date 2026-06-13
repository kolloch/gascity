package config

import (
	"errors"
	"strings"
	"testing"
)

// realisticCity mirrors the shape of a production city.toml: a [workspace]
// table, a top-level array value, multiple [[rigs]] each with sub-tables
// ([rigs.imports], [[rigs.patches]] with multi-line-ish inline arrays) and a
// commented [rigs.formula_vars] block. It exercises the scanner against
// comments, blank lines, array values, and key ordering.
const realisticCity = `[workspace]
provider = "claude"

[providers]
[providers.claude-rotating]
base = "builtin:claude"
command = "claude-rotating"

[[rigs]]
name = "zack"
prefix = "za"
default_branch = "main"
[rigs.imports]
[rigs.imports.gastown]
source = "./.gc/system/packs/gastown"

[[rigs.patches]]
agent = "polecat"
pre_start_append = ["a", "b", "c"]
max_active_sessions = 2

# Pre-merge CI gate for zack — these rig-scoped vars feed the polecat and
# refinery. Keep this rationale block intact across config mutations.
[rigs.formula_vars]
build_command = "cargo build --workspace"
lint_command = "cargo clippy --workspace"

[[rigs]]
name = "gas-ui"
default_branch = "main"
[rigs.imports]
[rigs.imports.gastown]
source = "./.gc/system/packs/gastown"
`

func TestSetRigSuspendedInPlace_InsertAndRemoveRoundTrip(t *testing.T) {
	original := []byte(realisticCity)

	suspended, err := SetRigSuspendedInPlace(original, "zack", true)
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}

	// Only one line added, and it carries the intended value.
	if got := lineDelta(string(original), string(suspended)); got != 1 {
		t.Fatalf("suspend changed %d net lines, want exactly 1 added\n%s", got, suspended)
	}
	if !strings.Contains(string(suspended), "suspended = true") {
		t.Fatalf("suspended output missing 'suspended = true':\n%s", suspended)
	}
	// Every comment and the formula_vars rationale survive verbatim.
	for _, frag := range []string{
		"# Pre-merge CI gate for zack",
		"Keep this rationale block intact",
		`build_command = "cargo build --workspace"`,
		`pre_start_append = ["a", "b", "c"]`,
		"[rigs.formula_vars]",
	} {
		if !strings.Contains(string(suspended), frag) {
			t.Errorf("suspend dropped fragment %q:\n%s", frag, suspended)
		}
	}

	// The suspended key lands inside the zack rig's direct body, before its
	// first sub-table.
	assertSuspendedUnderRig(t, string(suspended), "zack")

	resumed, err := SetRigSuspendedInPlace(suspended, "zack", false)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if string(resumed) != string(original) {
		t.Fatalf("suspend→resume not byte-identical.\n--- original ---\n%s\n--- resumed ---\n%s", original, resumed)
	}
}

func TestSetRigSuspendedInPlace_TogglesTargetRigOnly(t *testing.T) {
	original := []byte(realisticCity)
	out, err := SetRigSuspendedInPlace(original, "gas-ui", true)
	if err != nil {
		t.Fatalf("suspend gas-ui: %v", err)
	}
	cfg, err := Parse(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, r := range cfg.Rigs {
		want := r.Name == "gas-ui"
		if r.Suspended != want {
			t.Errorf("rig %q suspended=%v, want %v", r.Name, r.Suspended, want)
		}
	}
	assertSuspendedUnderRig(t, string(out), "gas-ui")
}

func TestSetRigSuspendedInPlace_SuspendIdempotent(t *testing.T) {
	once, err := SetRigSuspendedInPlace([]byte(realisticCity), "zack", true)
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	twice, err := SetRigSuspendedInPlace(once, "zack", true)
	if err != nil {
		t.Fatalf("suspend again: %v", err)
	}
	if string(twice) != string(once) {
		t.Fatalf("re-suspending changed bytes:\n%s", twice)
	}
}

func TestSetRigSuspendedInPlace_ResumeNotSuspendedIsNoop(t *testing.T) {
	original := []byte(realisticCity)
	out, err := SetRigSuspendedInPlace(original, "zack", false)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if string(out) != string(original) {
		t.Fatalf("resuming an unsuspended rig changed bytes:\n%s", out)
	}
}

func TestSetRigSuspendedInPlace_ExistingSuspendedFalseFlipsInPlace(t *testing.T) {
	// A hand-written `suspended = false` line is flipped in place, not moved
	// or duplicated, preserving surrounding keys.
	city := `[[rigs]]
name = "frontend"
prefix = "fe"
suspended = false
default_branch = "main"
`
	out, err := SetRigSuspendedInPlace([]byte(city), "frontend", true)
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	want := `[[rigs]]
name = "frontend"
prefix = "fe"
suspended = true
default_branch = "main"
`
	if string(out) != want {
		t.Fatalf("in-place flip mismatch:\n got: %q\nwant: %q", out, want)
	}
}

func TestSetRigSuspendedInPlace_NotFound(t *testing.T) {
	_, err := SetRigSuspendedInPlace([]byte(realisticCity), "nonexistent", true)
	if !errors.Is(err, ErrInPlaceTableNotFound) {
		t.Fatalf("err = %v, want ErrInPlaceTableNotFound", err)
	}
}

func TestSetRigSuspendedInPlace_MultilineArrayNotMistakenForHeader(t *testing.T) {
	// A multi-line array whose continuation lines begin with '[' must not be
	// read as table headers; otherwise the rig body boundary would be wrong.
	city := `[[rigs]]
name = "matrixrig"
grid = [
  [1, 2],
  [3, 4],
]
default_branch = "main"

[[rigs]]
name = "other"
`
	out, err := SetRigSuspendedInPlace([]byte(city), "matrixrig", true)
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	cfg, err := Parse(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !rigSuspended(cfg, "matrixrig") {
		t.Errorf("matrixrig not suspended:\n%s", out)
	}
	if rigSuspended(cfg, "other") {
		t.Errorf("wrong rig suspended:\n%s", out)
	}
	// The suspended key must sit inside matrixrig, after the array closes.
	if !strings.Contains(string(out), "]\nsuspended = true") && !strings.Contains(string(out), "default_branch = \"main\"\nsuspended = true") {
		t.Errorf("suspended key not placed in matrixrig body:\n%s", out)
	}
	// Round trip is clean.
	back, err := SetRigSuspendedInPlace(out, "matrixrig", false)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if string(back) != city {
		t.Fatalf("round trip not byte-identical:\n%s", back)
	}
}

func TestSetRigSuspendedInPlace_CRLFPreserved(t *testing.T) {
	city := "[[rigs]]\r\nname = \"winrig\"\r\ndefault_branch = \"main\"\r\n"
	out, err := SetRigSuspendedInPlace([]byte(city), "winrig", true)
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if !strings.Contains(string(out), "suspended = true\r\n") {
		t.Fatalf("CRLF terminator not used for inserted line:\n%q", out)
	}
	if strings.Contains(strings.ReplaceAll(string(out), "\r\n", ""), "\n") {
		t.Fatalf("stray LF introduced:\n%q", out)
	}
	back, err := SetRigSuspendedInPlace(out, "winrig", false)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if string(back) != city {
		t.Fatalf("CRLF round trip not byte-identical:\n%q", back)
	}
}

func TestSetRigSuspendedInPlace_NoTrailingNewlineStillValid(t *testing.T) {
	// Exotic: target rig is the last table and the file has no trailing
	// newline. We guarantee correctness (valid TOML, right value), not strict
	// byte identity of the inserted boundary.
	city := "[[rigs]]\nname = \"tail\""
	out, err := SetRigSuspendedInPlace([]byte(city), "tail", true)
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	cfg, err := Parse(out)
	if err != nil {
		t.Fatalf("parse produced invalid TOML: %v\n%q", err, out)
	}
	if !rigSuspended(cfg, "tail") {
		t.Fatalf("tail rig not suspended:\n%q", out)
	}
}

func TestSetWorkspaceSuspendedInPlace_RoundTrip(t *testing.T) {
	original := []byte(realisticCity)
	suspended, err := SetWorkspaceSuspendedInPlace(original, true)
	if err != nil {
		t.Fatalf("suspend city: %v", err)
	}
	cfg, err := Parse(suspended)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cfg.Workspace.Suspended {
		t.Fatalf("workspace not suspended:\n%s", suspended)
	}
	// The key belongs to [workspace], i.e. before the [providers] table.
	wsIdx := strings.Index(string(suspended), "[workspace]")
	provIdx := strings.Index(string(suspended), "[providers]")
	keyIdx := strings.Index(string(suspended), "suspended = true")
	if wsIdx >= keyIdx || keyIdx >= provIdx {
		t.Fatalf("suspended key not inside [workspace] body:\n%s", suspended)
	}

	resumed, err := SetWorkspaceSuspendedInPlace(suspended, false)
	if err != nil {
		t.Fatalf("resume city: %v", err)
	}
	if string(resumed) != string(original) {
		t.Fatalf("workspace suspend→resume not byte-identical:\n%s", resumed)
	}
}

func TestSetWorkspaceSuspendedInPlace_NotFound(t *testing.T) {
	city := `[[rigs]]
name = "lonely"
`
	_, err := SetWorkspaceSuspendedInPlace([]byte(city), true)
	if !errors.Is(err, ErrInPlaceTableNotFound) {
		t.Fatalf("err = %v, want ErrInPlaceTableNotFound", err)
	}
}

func TestSetWorkspaceSuspendedInPlace_ResumeRemovesLine(t *testing.T) {
	city := `[workspace]
name = "test-city"
suspended = true
`
	out, err := SetWorkspaceSuspendedInPlace([]byte(city), false)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if strings.Contains(string(out), "suspended") {
		t.Fatalf("resume left a suspended line behind:\n%s", out)
	}
	want := `[workspace]
name = "test-city"
`
	if string(out) != want {
		t.Fatalf("resume mismatch:\n got: %q\nwant: %q", out, want)
	}
}

// --- helpers ---

func rigSuspended(c *City, name string) bool {
	for _, r := range c.Rigs {
		if r.Name == name {
			return r.Suspended
		}
	}
	return false
}

// lineDelta returns the net change in line count between before and after.
func lineDelta(before, after string) int {
	return strings.Count(after, "\n") - strings.Count(before, "\n")
}

// assertSuspendedUnderRig checks that a `suspended = true` line appears within
// the named rig's [[rigs]] block (before the next [ header following it).
func assertSuspendedUnderRig(t *testing.T, doc, rig string) {
	t.Helper()
	lines := strings.Split(doc, "\n")
	inRig := false
	for _, ln := range lines {
		trimmed := strings.TrimLeft(ln, " \t")
		if strings.HasPrefix(trimmed, "[[rigs]]") {
			inRig = false // reset; confirm by name below
		}
		if strings.HasPrefix(trimmed, "[") && inRig {
			// Entered a sub-table or next table; the rig's direct body ended.
			if !strings.HasPrefix(trimmed, "[rigs.") {
				inRig = false
			}
		}
		if strings.HasPrefix(trimmed, "name = ") {
			if v, ok := parseTOMLStringValue(strings.TrimSpace(strings.TrimPrefix(trimmed, "name ="))); ok && v == rig {
				inRig = true
			}
		}
		if inRig && isSuspendedKeyLine(trimmed) {
			return
		}
	}
	t.Fatalf("no suspended line found in rig %q body:\n%s", rig, doc)
}
