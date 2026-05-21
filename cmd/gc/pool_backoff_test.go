package main

import (
	"testing"
	"time"
)

func TestPoolBackoffStateApplyNoUnderfullPassesThrough(t *testing.T) {
	t.Parallel()
	b := NewPoolBackoffState(10 * time.Minute)
	now := time.Now()

	scale := map[string]int{"rig/template": 3}
	claim := map[string]int{"rig/template": 3}
	adjusted, decisions := b.Apply(scale, claim, now)
	if got, want := adjusted["rig/template"], 3; got != want {
		t.Fatalf("adjusted[template] = %d, want %d", got, want)
	}
	if len(decisions) != 0 {
		t.Fatalf("expected no decisions when not underfull, got %+v", decisions)
	}
}

func TestPoolBackoffStateApplyClaimableHigherPassesThrough(t *testing.T) {
	t.Parallel()
	b := NewPoolBackoffState(10 * time.Minute)
	now := time.Now()

	scale := map[string]int{"rig/template": 2}
	claim := map[string]int{"rig/template": 5}
	adjusted, decisions := b.Apply(scale, claim, now)
	if got, want := adjusted["rig/template"], 2; got != want {
		t.Fatalf("adjusted[template] = %d, want %d", got, want)
	}
	if len(decisions) != 0 {
		t.Fatalf("expected no decisions when claimable exceeds scale, got %+v", decisions)
	}
}

func TestPoolBackoffStateApplyFirstUnderfullObserves(t *testing.T) {
	t.Parallel()
	b := NewPoolBackoffState(10 * time.Minute)
	now := time.Now()

	adjusted, decisions := b.Apply(
		map[string]int{"rig/template": 5},
		map[string]int{"rig/template": 2},
		now,
	)
	if got, want := adjusted["rig/template"], 5; got != want {
		t.Fatalf("first underfull tick should not suppress yet: adjusted = %d, want %d", got, want)
	}
	if len(decisions) != 1 {
		t.Fatalf("expected one observation decision, got %+v", decisions)
	}
	d := decisions[0]
	if d.Suppressed {
		t.Fatalf("expected suppressed=false on first observation, got %+v", d)
	}
	if d.Template != "rig/template" {
		t.Fatalf("unexpected template: %s", d.Template)
	}
	if d.ScaleCheck != 5 || d.Claimable != 2 || d.AdjustedDemand != 5 {
		t.Fatalf("unexpected counts in decision: %+v", d)
	}
}

func TestPoolBackoffStateApplyWithinCooldownStillAllowsSpawn(t *testing.T) {
	t.Parallel()
	b := NewPoolBackoffState(10 * time.Minute)
	now := time.Now()

	_, _ = b.Apply(map[string]int{"rig/template": 5}, map[string]int{"rig/template": 2}, now)
	adjusted, decisions := b.Apply(
		map[string]int{"rig/template": 5},
		map[string]int{"rig/template": 2},
		now.Add(5*time.Minute),
	)
	if got, want := adjusted["rig/template"], 5; got != want {
		t.Fatalf("within cooldown should not suppress: adjusted = %d, want %d", got, want)
	}
	if len(decisions) != 1 || decisions[0].Suppressed {
		t.Fatalf("expected non-suppressing observation within cooldown, got %+v", decisions)
	}
	if decisions[0].UnderfullSince != 5*time.Minute {
		t.Fatalf("expected underfull duration 5m, got %v", decisions[0].UnderfullSince)
	}
}

func TestPoolBackoffStateApplyAfterCooldownSuppresses(t *testing.T) {
	t.Parallel()
	b := NewPoolBackoffState(10 * time.Minute)
	now := time.Now()

	_, _ = b.Apply(map[string]int{"rig/template": 5}, map[string]int{"rig/template": 2}, now)
	adjusted, decisions := b.Apply(
		map[string]int{"rig/template": 5},
		map[string]int{"rig/template": 2},
		now.Add(10*time.Minute),
	)
	if got, want := adjusted["rig/template"], 2; got != want {
		t.Fatalf("after cooldown should cap at claimable: adjusted = %d, want %d", got, want)
	}
	if len(decisions) != 1 || !decisions[0].Suppressed {
		t.Fatalf("expected suppressed decision after cooldown, got %+v", decisions)
	}
	if decisions[0].AdjustedDemand != 2 {
		t.Fatalf("expected adjusted demand=2, got %d", decisions[0].AdjustedDemand)
	}
}

func TestPoolBackoffStateApplyClaimableRiseClearsState(t *testing.T) {
	t.Parallel()
	b := NewPoolBackoffState(10 * time.Minute)
	now := time.Now()

	// Engage back-off.
	_, _ = b.Apply(map[string]int{"rig/template": 5}, map[string]int{"rig/template": 2}, now)
	_, _ = b.Apply(map[string]int{"rig/template": 5}, map[string]int{"rig/template": 2}, now.Add(11*time.Minute))

	// Claimable rises — back-off clears.
	adjusted, decisions := b.Apply(
		map[string]int{"rig/template": 5},
		map[string]int{"rig/template": 5},
		now.Add(12*time.Minute),
	)
	if got, want := adjusted["rig/template"], 5; got != want {
		t.Fatalf("after claimable rise: adjusted = %d, want %d", got, want)
	}
	if len(decisions) != 0 {
		t.Fatalf("expected no decisions after clear, got %+v", decisions)
	}

	// Re-observe underfull — timer restarts, no immediate suppression.
	adjusted, decisions = b.Apply(
		map[string]int{"rig/template": 5},
		map[string]int{"rig/template": 2},
		now.Add(13*time.Minute),
	)
	if got, want := adjusted["rig/template"], 5; got != want {
		t.Fatalf("after re-underfull (timer restarted) adjusted = %d, want %d", got, want)
	}
	if len(decisions) != 1 || decisions[0].Suppressed {
		t.Fatalf("expected non-suppressing observation after timer restart, got %+v", decisions)
	}
}

func TestPoolBackoffStateApplyMissingClaimableSignalPassesThrough(t *testing.T) {
	t.Parallel()
	b := NewPoolBackoffState(10 * time.Minute)
	now := time.Now()

	// scale_check has a count but claimable map has no entry — could happen
	// when claimable computation failed for this template.
	adjusted, decisions := b.Apply(
		map[string]int{"rig/template": 5},
		map[string]int{},
		now,
	)
	if got, want := adjusted["rig/template"], 5; got != want {
		t.Fatalf("missing claimable signal should pass through: adjusted = %d, want %d", got, want)
	}
	if len(decisions) != 0 {
		t.Fatalf("expected no decisions when claimable signal absent, got %+v", decisions)
	}
}

func TestPoolBackoffStateApplyZeroClaimableSuppressesAfterCooldown(t *testing.T) {
	t.Parallel()
	b := NewPoolBackoffState(10 * time.Minute)
	now := time.Now()

	_, _ = b.Apply(map[string]int{"rig/template": 3}, map[string]int{"rig/template": 0}, now)
	adjusted, decisions := b.Apply(
		map[string]int{"rig/template": 3},
		map[string]int{"rig/template": 0},
		now.Add(11*time.Minute),
	)
	if got, want := adjusted["rig/template"], 0; got != want {
		t.Fatalf("zero claimable should suppress to 0 after cooldown: adjusted = %d, want %d", got, want)
	}
	if len(decisions) != 1 || !decisions[0].Suppressed {
		t.Fatalf("expected suppressed decision, got %+v", decisions)
	}
}

func TestPoolBackoffStatePerTemplateIndependent(t *testing.T) {
	t.Parallel()
	b := NewPoolBackoffState(10 * time.Minute)
	now := time.Now()

	// Template A underfull, template B fully satisfied.
	_, _ = b.Apply(
		map[string]int{"a": 5, "b": 3},
		map[string]int{"a": 2, "b": 3},
		now,
	)
	adjusted, decisions := b.Apply(
		map[string]int{"a": 5, "b": 3},
		map[string]int{"a": 2, "b": 3},
		now.Add(11*time.Minute),
	)
	if adjusted["a"] != 2 {
		t.Fatalf("expected template a suppressed to 2, got %d", adjusted["a"])
	}
	if adjusted["b"] != 3 {
		t.Fatalf("expected template b unchanged at 3, got %d", adjusted["b"])
	}
	// Only template a should appear in decisions (b is satisfied).
	aDecisions := 0
	for _, d := range decisions {
		if d.Template == "a" {
			aDecisions++
			if !d.Suppressed {
				t.Fatalf("expected template a suppressed, got %+v", d)
			}
		}
		if d.Template == "b" {
			t.Fatalf("template b should not have a decision (not underfull), got %+v", d)
		}
	}
	if aDecisions != 1 {
		t.Fatalf("expected one decision for template a, got %d", aDecisions)
	}
}

func TestPoolBackoffStateNilReceiverSafe(t *testing.T) {
	t.Parallel()
	var b *PoolBackoffState
	scale := map[string]int{"x": 1}
	claim := map[string]int{"x": 0}
	// Nil receiver should not panic; should pass through unchanged.
	adjusted, decisions := b.Apply(scale, claim, time.Now())
	if got, want := adjusted["x"], 1; got != want {
		t.Fatalf("nil receiver should pass through: adjusted = %d, want %d", got, want)
	}
	if len(decisions) != 0 {
		t.Fatalf("nil receiver should return no decisions, got %+v", decisions)
	}
}

func TestPoolBackoffStateApplyEmptyMapsSafe(t *testing.T) {
	t.Parallel()
	b := NewPoolBackoffState(10 * time.Minute)
	adjusted, decisions := b.Apply(nil, nil, time.Now())
	if len(adjusted) != 0 {
		t.Fatalf("empty inputs should produce empty output, got %+v", adjusted)
	}
	if len(decisions) != 0 {
		t.Fatalf("empty inputs should produce no decisions, got %+v", decisions)
	}
}

func TestPoolBackoffStateDefaultCooldown(t *testing.T) {
	t.Parallel()
	// NewPoolBackoffState with non-positive cooldown uses the package default.
	b := NewPoolBackoffState(0)
	if b.cooldown != defaultPoolBackoffCooldown {
		t.Fatalf("expected default cooldown %v, got %v", defaultPoolBackoffCooldown, b.cooldown)
	}
	b2 := NewPoolBackoffState(-time.Second)
	if b2.cooldown != defaultPoolBackoffCooldown {
		t.Fatalf("negative cooldown should fall back to default; got %v", b2.cooldown)
	}
}
