package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// TestBuildDesiredState_PoolBootstrapsWhenAllSessionBeadsHaveBeenSwept covers
// the ga-u3q symptom: many session beads exist but every one has been retired
// via the sweep / orphan pipeline (state=gc_swept, status="open" because a
// partial close never reached bd close). The reconciler must not pick the
// terminal-released bead as a "reusable" candidate — it must plan a fresh
// pool create so the routed work actually gets claimed.
//
// Without the fix, reusablePoolSessionBead returns true for status=open
// state=gc_swept beads, and selectOrPlanPoolSessionBead reuses the dead
// session instead of materializing a new one. The pool sits at zero live
// polecats forever while the routed bead waits.
func TestBuildDesiredState_PoolBootstrapsWhenAllSessionBeadsHaveBeenSwept(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	const template = "polecat"

	// Routed-open work that should drive demand.
	if _, err := store.Create(beads.Bead{
		Title:    "routed pool work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": template},
	}); err != nil {
		t.Fatalf("create routed work bead: %v", err)
	}

	// Multiple gc_swept session beads — half-closed: terminal metadata
	// set but status never advanced to "closed" (partial-close race, refs/dolt
	// sync hiccup, or stale cache). Same shape pe-r7c1 observed.
	for i := 0; i < 3; i++ {
		bead, err := store.Create(beads.Bead{
			Title:  template,
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel, "template:" + template},
			Metadata: map[string]string{
				"template":             template,
				"agent_name":           template,
				"state":                "gc_swept",
				poolManagedMetadataKey: boolMetadata(true),
			},
		})
		if err != nil {
			t.Fatalf("create swept session bead %d: %v", i, err)
		}
		if err := store.SetMetadata(bead.ID, "session_name", PoolSessionName(template, bead.ID)); err != nil {
			t.Fatalf("set session_name: %v", err)
		}
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              template,
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
		}},
	}

	snapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}

	var stderr strings.Builder
	dsResult := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		store, nil, snapshot, nil, nil, &stderr,
	)

	if got := dsResult.ScaleCheckCounts[template]; got != 1 {
		t.Fatalf("ScaleCheckCounts[%s] = %d, want 1 (one routed-open bead)", template, got)
	}

	// The desired state must contain at least one live pool session for the
	// template — that's the bootstrap the routed work needs.
	desiredLive := 0
	for _, tp := range dsResult.State {
		if tp.TemplateName != template {
			continue
		}
		// gc_swept beads must not be "preserved" as desired sessions —
		// they are dead. Only a fresh, non-terminal bead counts.
		if strings.TrimSpace(tp.SessionName) == "" {
			continue
		}
		desiredLive++
	}
	if desiredLive < 1 {
		t.Fatalf("desired live %s sessions = %d, want >=1; gc_swept beads stranded the pool. stderr:\n%s", template, desiredLive, stderr.String())
	}

	// And the store should now have at least one new, non-terminal session
	// bead — confirming the scaler actually planned a fresh create.
	sessions, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("load session beads: %v", err)
	}
	fresh := 0
	for _, sb := range sessions {
		if strings.TrimSpace(sb.Metadata["template"]) != template {
			continue
		}
		if session.TerminalStateReleased(sb) {
			continue
		}
		fresh++
	}
	if fresh < 1 {
		t.Fatalf("non-terminal pool session beads = %d, want >=1 (scaler must bootstrap past swept beads); stderr:\n%s", fresh, stderr.String())
	}
}

// TestBuildDesiredState_PoolBootstrapsWhenAllSessionBeadsAreClosed covers the
// canonical clean state: every prior session bead is fully closed
// (status="closed"), and one routed-open work bead is waiting. The scaler
// must spawn a fresh session. This case is already expected to pass — it
// guards the simple "rate-limit cleanup removed all sessions" recovery path.
func TestBuildDesiredState_PoolBootstrapsWhenAllSessionBeadsAreClosed(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	const template = "polecat"

	if _, err := store.Create(beads.Bead{
		Title:    "routed pool work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": template},
	}); err != nil {
		t.Fatalf("create routed work bead: %v", err)
	}

	// Two prior session beads, both fully closed via Update so status
	// actually flips.
	for i := 0; i < 2; i++ {
		bead, err := store.Create(beads.Bead{
			Title:  template,
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel, "template:" + template},
			Metadata: map[string]string{
				"template":             template,
				"agent_name":           template,
				"state":                "active",
				poolManagedMetadataKey: boolMetadata(true),
			},
		})
		if err != nil {
			t.Fatalf("create prior session bead %d: %v", i, err)
		}
		if err := store.SetMetadata(bead.ID, "session_name", PoolSessionName(template, bead.ID)); err != nil {
			t.Fatalf("set session_name: %v", err)
		}
		closedStatus := "closed"
		if err := store.Update(bead.ID, beads.UpdateOpts{Status: &closedStatus}); err != nil {
			t.Fatalf("close prior session bead: %v", err)
		}
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              template,
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
		}},
	}

	snapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}

	var stderr strings.Builder
	dsResult := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		store, nil, snapshot, nil, nil, &stderr,
	)

	if got := dsResult.ScaleCheckCounts[template]; got != 1 {
		t.Fatalf("ScaleCheckCounts[%s] = %d, want 1 (one routed-open bead)", template, got)
	}
	desired := 0
	for _, tp := range dsResult.State {
		if tp.TemplateName == template {
			desired++
		}
	}
	if desired != 1 {
		t.Fatalf("desired %s sessions = %d, want 1 (fresh bootstrap after all-closed history); stderr:\n%s", template, desired, stderr.String())
	}
}
