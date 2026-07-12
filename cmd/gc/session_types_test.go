package main

import (
	"testing"
	"time"

	sessions "github.com/gastownhall/gascity/internal/session"
)

func TestWakeReason_Constants(t *testing.T) {
	reasons := []WakeReason{WakeConfig, WakeAttached, WakeWait, WakeWork}
	seen := map[WakeReason]bool{}
	for _, r := range reasons {
		if seen[r] {
			t.Fatalf("duplicate WakeReason: %s", r)
		}
		seen[r] = true
	}
}

func TestSessionNamePattern(t *testing.T) {
	valid := []string{
		"mayor",
		"test-city-mayor",
		"worker-3",
		"agent_1",
		"A1",
		"x",
	}
	invalid := []string{
		"",
		"-starts-with-dash",
		"_starts-with-underscore",
		"has spaces",
		"has.dots",
		"has/slash",
		"has;semicolon",
		"has$dollar",
		"../traversal",
	}

	for _, name := range valid {
		if !sessions.IsSessionNameSyntaxValid(name) {
			t.Errorf("expected valid session name: %q", name)
		}
	}
	for _, name := range invalid {
		if sessions.IsSessionNameSyntaxValid(name) {
			t.Errorf("expected invalid session name: %q", name)
		}
	}
}

func TestDrainTracker(t *testing.T) {
	dt := newDrainTracker()

	// Initially empty.
	if dt.get("bead-1") != nil {
		t.Fatal("expected nil for unknown bead")
	}

	// Set and get.
	now := time.Now()
	ds := &drainState{
		startedAt:  now,
		deadline:   now.Add(5 * time.Minute),
		reason:     "idle",
		generation: 3,
	}
	dt.set("bead-1", ds)

	got := dt.get("bead-1")
	if got == nil {
		t.Fatal("expected drain state")
	}
	if got.reason != "idle" {
		t.Errorf("reason = %q, want %q", got.reason, "idle")
	}
	if got.generation != 3 {
		t.Errorf("generation = %d, want %d", got.generation, 3)
	}

	// All returns a copy.
	all := dt.all()
	if len(all) != 1 {
		t.Fatalf("all() returned %d entries, want 1", len(all))
	}

	// Remove.
	dt.remove("bead-1")
	if dt.get("bead-1") != nil {
		t.Fatal("expected nil after remove")
	}
	if len(dt.all()) != 0 {
		t.Fatal("expected empty after remove")
	}
}

func TestDrainTracker_ObserveLivenessUnknown_ThrottleCooldown(t *testing.T) {
	dt := newDrainTracker()
	// Deliberately a non-default cooldown (production uses
	// livenessUnknownLogCooldown) so the throttle window is exercised as a real
	// parameter rather than a hardcoded constant.
	const cooldown = 2 * time.Minute
	base := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

	// First failure always logs.
	logNow, escalate, consecutive := dt.observeLivenessUnknown("bead-1", base, cooldown, 5)
	if !logNow || escalate || consecutive != 1 {
		t.Fatalf("first observation: logNow=%v escalate=%v consecutive=%d; want true false 1", logNow, escalate, consecutive)
	}

	// A second failure inside the cooldown window is throttled.
	logNow, _, consecutive = dt.observeLivenessUnknown("bead-1", base.Add(30*time.Second), cooldown, 5)
	if logNow {
		t.Errorf("observation within cooldown logged; want throttled")
	}
	if consecutive != 2 {
		t.Errorf("consecutive = %d, want 2", consecutive)
	}

	// Just before the cooldown elapses: still throttled.
	if logNow, _, _ = dt.observeLivenessUnknown("bead-1", base.Add(cooldown-time.Nanosecond), cooldown, 5); logNow {
		t.Errorf("observation just before cooldown logged; want throttled")
	}

	// At the cooldown boundary: logs again and resets the window.
	if logNow, _, _ = dt.observeLivenessUnknown("bead-1", base.Add(cooldown), cooldown, 5); !logNow {
		t.Errorf("observation at cooldown boundary throttled; want logged")
	}
	// Immediately after that log: throttled again against the new anchor.
	if logNow, _, _ = dt.observeLivenessUnknown("bead-1", base.Add(cooldown+time.Second), cooldown, 5); logNow {
		t.Errorf("observation right after a log logged again; want throttled")
	}

	// A different session throttles independently.
	if logNow, _, consecutive = dt.observeLivenessUnknown("bead-2", base.Add(cooldown), cooldown, 5); !logNow || consecutive != 1 {
		t.Errorf("independent session: logNow=%v consecutive=%d; want true 1", logNow, consecutive)
	}
}

func TestDrainTracker_ObserveLivenessUnknown_EscalationThreshold(t *testing.T) {
	dt := newDrainTracker()
	const cooldown = 5 * time.Minute
	const threshold = 5
	base := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

	// Below the threshold: never escalates.
	for i := 1; i < threshold; i++ {
		_, escalate, consecutive := dt.observeLivenessUnknown("bead-1", base.Add(time.Duration(i)*time.Second), cooldown, threshold)
		if escalate {
			t.Fatalf("escalated early at observation %d (consecutive=%d)", i, consecutive)
		}
	}

	// Crossing the threshold escalates exactly once.
	_, escalate, consecutive := dt.observeLivenessUnknown("bead-1", base.Add(time.Duration(threshold)*time.Second), cooldown, threshold)
	if !escalate {
		t.Fatalf("did not escalate at threshold (consecutive=%d)", consecutive)
	}
	if consecutive != threshold {
		t.Errorf("consecutive = %d, want %d", consecutive, threshold)
	}

	// Subsequent failures do not re-escalate (latched for the episode).
	if _, escalate, _ = dt.observeLivenessUnknown("bead-1", base.Add(time.Duration(threshold+1)*time.Second), cooldown, threshold); escalate {
		t.Errorf("re-escalated after latch; want single escalation per episode")
	}

	// A threshold of 0 disables escalation entirely.
	for i := 0; i < threshold+3; i++ {
		if _, escalate, _ = dt.observeLivenessUnknown("bead-3", base.Add(time.Duration(i)*time.Second), cooldown, 0); escalate {
			t.Fatalf("escalated with threshold=0 at observation %d", i)
		}
	}
}

func TestDrainTracker_ObserveLivenessUnknown_EscalationResetsCooldown(t *testing.T) {
	dt := newDrainTracker()
	const cooldown = 5 * time.Minute
	const threshold = 2
	base := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

	// t0: first failure logs (consecutive=1), sets the cooldown anchor.
	if logNow, escalate, _ := dt.observeLivenessUnknown("bead-1", base, cooldown, threshold); !logNow || escalate {
		t.Fatalf("t0: logNow=%v escalate=%v; want true false", logNow, escalate)
	}
	// t0+1s: crosses threshold and escalates even though within the log cooldown.
	if logNow, escalate, _ := dt.observeLivenessUnknown("bead-1", base.Add(time.Second), cooldown, threshold); logNow || !escalate {
		t.Fatalf("threshold tick: logNow=%v escalate=%v; want false true", logNow, escalate)
	}
	// The escalation must reset the cooldown anchor: a skip line 1s later stays throttled.
	if logNow, _, _ := dt.observeLivenessUnknown("bead-1", base.Add(2*time.Second), cooldown, threshold); logNow {
		t.Errorf("logged right after escalation; escalation should reset the cooldown anchor")
	}
}

func TestDrainTracker_ClearLivenessUnknown_Resets(t *testing.T) {
	dt := newDrainTracker()
	const cooldown = 5 * time.Minute
	const threshold = 3
	base := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

	for i := 0; i < threshold; i++ {
		dt.observeLivenessUnknown("bead-1", base.Add(time.Duration(i)*time.Second), cooldown, threshold)
	}
	// A successful observation clears the run.
	dt.clearLivenessUnknown("bead-1")

	// Next failure behaves like a fresh episode: logs, counts from 1, can escalate again.
	logNow, escalate, consecutive := dt.observeLivenessUnknown("bead-1", base.Add(time.Minute), cooldown, threshold)
	if !logNow || escalate || consecutive != 1 {
		t.Fatalf("post-clear: logNow=%v escalate=%v consecutive=%d; want true false 1", logNow, escalate, consecutive)
	}
	for i := 1; i < threshold; i++ {
		_, escalate, _ = dt.observeLivenessUnknown("bead-1", base.Add(time.Minute+time.Duration(i)*time.Second), cooldown, threshold)
	}
	if !escalate {
		t.Errorf("post-clear episode did not escalate again at threshold")
	}
}

func TestDrainTracker_ObserveLivenessUnknown_NilSafe(t *testing.T) {
	var dt *drainTracker
	logNow, escalate, consecutive := dt.observeLivenessUnknown("bead-1", time.Now(), 5*time.Minute, 5)
	if !logNow || escalate || consecutive != 0 {
		t.Errorf("nil tracker: logNow=%v escalate=%v consecutive=%d; want true false 0", logNow, escalate, consecutive)
	}
	dt.clearLivenessUnknown("bead-1") // must not panic
}

func TestExecSpec_ZeroValue(t *testing.T) {
	var spec ExecSpec
	if spec.Path != "" || spec.WorkDir != "" {
		t.Error("zero-value ExecSpec should have empty fields")
	}
	if spec.Args != nil {
		t.Error("zero-value Args should be nil")
	}
	if spec.Env != nil {
		t.Error("zero-value Env should be nil")
	}
}

func TestReconcilerDefaults(t *testing.T) {
	if stabilityThreshold != 30*time.Second {
		t.Errorf("stabilityThreshold = %v, want 30s", stabilityThreshold)
	}
	if defaultMaxWakesPerTick != 5 {
		t.Errorf("defaultMaxWakesPerTick = %d, want 5", defaultMaxWakesPerTick)
	}
	if defaultTickBudget != 5*time.Second {
		t.Errorf("defaultTickBudget = %v, want 5s", defaultTickBudget)
	}
	if orphanGraceTicks != 3 {
		t.Errorf("orphanGraceTicks = %d, want 3", orphanGraceTicks)
	}
	if defaultDrainTimeout != 5*time.Minute {
		t.Errorf("defaultDrainTimeout = %v, want 5m", defaultDrainTimeout)
	}
	if defaultQuarantineDuration != 5*time.Minute {
		t.Errorf("defaultQuarantineDuration = %v, want 5m", defaultQuarantineDuration)
	}
	if defaultMaxWakeAttempts != 5 {
		t.Errorf("defaultMaxWakeAttempts = %d, want 5", defaultMaxWakeAttempts)
	}
	if livenessUnknownLogCooldown != 5*time.Minute {
		t.Errorf("livenessUnknownLogCooldown = %v, want 5m", livenessUnknownLogCooldown)
	}
	if livenessUnknownEscalateThreshold != 5 {
		t.Errorf("livenessUnknownEscalateThreshold = %d, want 5", livenessUnknownEscalateThreshold)
	}
}
