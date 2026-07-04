// Package gastown_test — polecat claim-race guard.
//
// Split into its own file so the guard for ga-x9lr does not force a
// whole-file re-lint of gastown_test.go (which carries unrelated
// pre-existing staticcheck findings tracked in ga-1119).
package gastown_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPolecatPromptDrainsOnFailedClaim guards the fix for ga-x9lr: the
// duplicate-dispatch race where a bead reopened after a stale-claim takeover
// was ground by two polecats at once. bd's `--claim` is already a
// compare-and-swap that yields exactly one winner (it updates only while the
// issue is still open and its assignee is empty, NULL, or already the caller;
// ErrAlreadyClaimed otherwise), so the harm did not come from a non-atomic
// claim. It came from the LOSER of the claim race
// working the bead anyway, because the prompt treated `--claim` as an
// always-succeeding "grab". The prompt must tell a polecat that a failed claim
// means the bead is not theirs — do not work it, drain like an empty hook —
// so only the CAS winner proceeds.
func TestPolecatPromptDrainsOnFailedClaim(t *testing.T) {
	dir := exampleDir()
	path := filepath.Join(dir, "packs", "gastown", "agents", "polecat", "prompt.template.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading polecat prompt: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		"compare-and-swap you can lose",
		"already claimed by",
		"duplicate-dispatch bug",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("polecat prompt missing failed-claim drain guidance %q:\n%s", want, body)
		}
	}
}
