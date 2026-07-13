package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

func TestSessionBeadAssigneeIdentities(t *testing.T) {
	tests := []struct {
		name string
		bead beads.Bead
		want []string
	}{
		{
			name: "empty bead produces no identities",
			bead: beads.Bead{},
			want: []string{},
		},
		{
			name: "id only",
			bead: beads.Bead{ID: "mc-xyz"},
			want: []string{"mc-xyz"},
		},
		{
			name: "session_name only",
			bead: beads.Bead{Metadata: map[string]string{"session_name": "worker-mc-live"}},
			want: []string{"worker-mc-live"},
		},
		{
			name: "configured_named_identity only",
			bead: beads.Bead{Metadata: map[string]string{"configured_named_identity": "reviewer"}},
			want: []string{"reviewer"},
		},
		{
			name: "alias only",
			bead: beads.Bead{Metadata: map[string]string{"alias": "nux"}},
			want: []string{"nux"},
		},
		{
			name: "alias_history single entry",
			bead: beads.Bead{Metadata: map[string]string{"alias_history": "previous"}},
			want: []string{"previous"},
		},
		{
			name: "alias_history multiple entries",
			bead: beads.Bead{Metadata: map[string]string{"alias_history": "first,second,third"}},
			want: []string{"first", "second", "third"},
		},
		{
			name: "all fields populated",
			bead: beads.Bead{
				ID: "mc-xyz",
				Metadata: map[string]string{
					"session_name":              "worker-mc-live",
					"configured_named_identity": "reviewer",
					"alias":                     "rictus",
					"alias_history":             "nux",
				},
			},
			want: []string{"mc-xyz", "worker-mc-live", "reviewer", "rictus", "nux"},
		},
		{
			name: "whitespace-only values are trimmed and skipped",
			bead: beads.Bead{
				ID: "  ",
				Metadata: map[string]string{
					"session_name":              "   ",
					"configured_named_identity": "\t",
					"alias":                     " ",
					"alias_history":             "  ,  , real ,  ",
				},
			},
			want: []string{"real"},
		},
		{
			name: "values with surrounding whitespace are trimmed",
			bead: beads.Bead{
				ID: "  mc-xyz  ",
				Metadata: map[string]string{
					"session_name":              "  worker-mc-live  ",
					"configured_named_identity": "  reviewer  ",
					"alias":                     "  nux  ",
				},
			},
			want: []string{"mc-xyz", "worker-mc-live", "reviewer", "nux"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sessionBeadAssigneeIdentities(tt.bead)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d identities %v, want %d %v", len(got), got, len(tt.want), tt.want)
			}
			for i, id := range got {
				if id != tt.want[i] {
					t.Errorf("identity[%d] = %q, want %q (full got=%v, want=%v)", i, id, tt.want[i], got, tt.want)
				}
			}
		})
	}
}

func TestPoolSessionName(t *testing.T) {
	tests := []struct {
		template string
		beadID   string
		want     string
	}{
		{"gascity/claude", "mc-xyz", "claude-mc-xyz"},
		{"claude", "mc-abc", "claude-mc-abc"},
		{"myrig/codex", "mc-123", "codex-mc-123"},
		{"control-dispatcher", "mc-wfc", "control-dispatcher-mc-wfc"},
		{"gs.polecat", "mc-dot", "gs__polecat-mc-dot"},
		{"myrig/gs.polecat", "mc-rigdot", "gs__polecat-mc-rigdot"},
	}
	for _, tt := range tests {
		got := PoolSessionName(tt.template, tt.beadID)
		if got != tt.want {
			t.Errorf("PoolSessionName(%q, %q) = %q, want %q", tt.template, tt.beadID, got, tt.want)
		}
	}
}

func TestGCSweepSessionBeads_ClosesOrphans(t *testing.T) {
	store := beads.NewMemStore()

	// Session bead with no assigned work.
	orphan, _ := store.Create(beads.Bead{Title: "orphan session", Type: "session"})

	// Session bead with assigned work.
	active, _ := store.Create(beads.Bead{Title: "active session", Type: "session"})
	workBead, _ := store.Create(beads.Bead{
		Title:    "work item",
		Assignee: active.ID,
		Status:   "in_progress",
	})
	_ = workBead

	sessionBeads := []beads.Bead{orphan, active}

	closed := GCSweepSessionBeads(store, nil, sessionBeads)

	if len(closed) != 1 {
		t.Fatalf("closed %d beads, want 1", len(closed))
	}
	if closed[0] != orphan.ID {
		t.Errorf("closed %q, want %q", closed[0], orphan.ID)
	}

	// Verify the orphan is actually closed in the store.
	got, _ := store.Get(orphan.ID)
	if got.Status != "closed" {
		t.Errorf("orphan status = %q, want closed", got.Status)
	}

	// Active session should still be open.
	got, _ = store.Get(active.ID)
	if got.Status == "closed" {
		t.Error("active session was closed, should stay open")
	}
}

func TestGCSweepSessionBeads_KeepsBlockedAssigned(t *testing.T) {
	store := beads.NewMemStore()

	sess, _ := store.Create(beads.Bead{
		Title:  "session",
		Type:   "session",
		Status: "open",
		Metadata: map[string]string{
			"state": "active",
		},
	})

	// Work bead is open (blocked) but assigned to this session.
	blocked, _ := store.Create(beads.Bead{
		Title:    "blocked work",
		Assignee: sess.ID,
		Status:   "open",
	})
	_ = blocked

	sessionBeads := []beads.Bead{sess}

	closed := GCSweepSessionBeads(store, nil, sessionBeads)

	if len(closed) != 0 {
		t.Errorf("closed %d beads, want 0 (blocked work keeps session alive)", len(closed))
	}
	got, err := store.Get(sess.ID)
	if err != nil {
		t.Fatalf("Get session bead: %v", err)
	}
	if got.Metadata["state"] != "active" {
		t.Fatalf("state = %q, want active when sweep skips close", got.Metadata["state"])
	}
}

func TestGCSweepSessionBeads_ClosesWhenAllWorkClosed(t *testing.T) {
	store := beads.NewMemStore()

	sess, _ := store.Create(beads.Bead{Title: "session", Type: "session"})

	// Work bead is closed — session has no remaining work.
	done, _ := store.Create(beads.Bead{
		Title:    "done work",
		Assignee: sess.ID,
	})
	_ = store.Close(done.ID)
	done, _ = store.Get(done.ID)

	sessionBeads := []beads.Bead{sess}

	closed := GCSweepSessionBeads(store, nil, sessionBeads)

	if len(closed) != 1 {
		t.Errorf("closed %d beads, want 1 (all work done)", len(closed))
	}
}

func TestGCSweepSessionBeads_SkipsAlreadyClosed(t *testing.T) {
	store := beads.NewMemStore()

	sess, _ := store.Create(beads.Bead{Title: "session", Type: "session"})
	_ = store.Close(sess.ID)
	sess, _ = store.Get(sess.ID)

	sessionBeads := []beads.Bead{sess}

	closed := GCSweepSessionBeads(store, nil, sessionBeads)

	if len(closed) != 0 {
		t.Errorf("closed %d beads, want 0 (already closed)", len(closed))
	}
}

func TestReleaseOrphanedPoolAssignments_ReopensMissingPoolAssignee(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "orphaned pool work",
		Assignee: "worker-dead",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		nil,
		[]beads.Bead{work},
		nil,
		nil,
		nil,
	)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s]", released, work.ID)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("status = %q, want open", got.Status)
	}
	if got.Assignee != "" {
		t.Fatalf("assignee = %q, want empty", got.Assignee)
	}
}

type sessionListMissStore struct {
	beads.Store
	directSessions map[string]beads.Bead
}

func (s sessionListMissStore) Get(id string) (beads.Bead, error) {
	if b, ok := s.directSessions[id]; ok {
		return b, nil
	}
	return s.Store.Get(id)
}

func (s sessionListMissStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Live && query.Label == sessionBeadLabel {
		return nil, nil
	}
	return s.Store.List(query)
}

func TestLiveSessionBeadExistsByIdentity_SkipsClosedSessionBead(t *testing.T) {
	// Regression: directSessionBeadIDCandidates can resolve to a session
	// bead that has since been closed. liveSessionBeadExistsByIdentity must
	// skip closed beads via the early continue so the caller falls through
	// to the live-list fallback instead of claiming the dead session is alive.
	base := beads.NewMemStore()
	closed, err := base.Create(beads.Bead{
		Title:  "closed session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	if err := base.Close(closed.ID); err != nil {
		t.Fatalf("Close session bead: %v", err)
	}
	closed, err = base.Get(closed.ID)
	if err != nil {
		t.Fatalf("Reload closed session: %v", err)
	}
	if closed.Status != "closed" {
		t.Fatalf("closed status = %q, want closed", closed.Status)
	}

	store := sessionListMissStore{
		Store:          base,
		directSessions: map[string]beads.Bead{"worker-mc-dead": closed},
	}

	if liveSessionBeadExistsByIdentity(store, "worker-mc-dead") {
		t.Error("liveSessionBeadExistsByIdentity = true, want false for closed session bead")
	}
}

func TestLiveSessionBeadExistsByIdentity_SkipsNonSessionBead(t *testing.T) {
	// Regression: directSessionBeadIDCandidates can resolve to a bead that
	// is not a session (e.g. a work bead whose ID collides with the
	// assignee string). liveSessionBeadExistsByIdentity must skip such
	// beads instead of treating them as live session owners.
	base := beads.NewMemStore()
	notSession, err := base.Create(beads.Bead{
		Title: "not a session",
		Type:  "task",
	})
	if err != nil {
		t.Fatalf("Create non-session bead: %v", err)
	}
	if notSession.Type == sessionBeadType {
		t.Fatalf("test setup: bead type = %q, want non-session", notSession.Type)
	}
	for _, label := range notSession.Labels {
		if label == sessionBeadLabel {
			t.Fatalf("test setup: bead has session label %q", label)
		}
	}

	store := sessionListMissStore{
		Store:          base,
		directSessions: map[string]beads.Bead{"worker-mc-task": notSession},
	}

	if liveSessionBeadExistsByIdentity(store, "worker-mc-task") {
		t.Error("liveSessionBeadExistsByIdentity = true, want false for non-session bead")
	}
}

func TestReleaseOrphanedPoolAssignments_SkipsLiveSessionMissingFromSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         "worker-mc-live",
			"template":             "worker",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "claimed pool work",
		Assignee: sessionBead.Metadata["session_name"],
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		nil,
		[]beads.Bead{work},
		[]beads.Store{store},
		nil,
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none for live session", released)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", got.Status)
	}
	if got.Assignee != sessionBead.Metadata["session_name"] {
		t.Fatalf("assignee = %q, want %q", got.Assignee, sessionBead.Metadata["session_name"])
	}
}

func TestReleaseOrphanedPoolAssignments_SkipsLiveSessionWhenLiveSessionListMissesIt(t *testing.T) {
	base := beads.NewMemStore()
	sessionBead, err := base.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         "worker-mc-live",
			"template":             "worker",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	work, err := base.Create(beads.Bead{
		Title:    "claimed pool work",
		Assignee: sessionBead.Metadata["session_name"],
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := base.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = base.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}
	store := sessionListMissStore{
		Store:          base,
		directSessions: map[string]beads.Bead{"mc-live": sessionBead},
	}

	released := releaseOrphanedPoolAssignments(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		nil,
		[]beads.Bead{work},
		[]beads.Store{store},
		nil,
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none for directly resolvable live session", released)
	}

	got, err := base.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", got.Status)
	}
	if got.Assignee != sessionBead.Metadata["session_name"] {
		t.Fatalf("assignee = %q, want %q", got.Assignee, sessionBead.Metadata["session_name"])
	}
}

func TestReleaseOrphanedPoolAssignments_SkipsLiveSessionAssignedByAlias(t *testing.T) {
	// Regression: polecat pool sessions carry their human-readable identity
	// in Metadata["alias"] (e.g. "nux"), separate from session_name
	// ("polecat-gc-vi6hhp"). Work claimed by a polecat is often assigned
	// under the alias, so orphan-release must recognize alias-owned work as
	// belonging to a live session.
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         "polecat-gc-vi6hhp",
			"alias":                "nux",
			"template":             "worker",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "claimed pool work",
		Assignee: "nux",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		[]beads.Bead{sessionBead},
		[]beads.Bead{work},
		[]beads.Store{store},
		nil,
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none — live polecat owns work via alias", released)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", got.Status)
	}
	if got.Assignee != "nux" {
		t.Fatalf("assignee = %q, want nux", got.Assignee)
	}
}

func TestReleaseOrphanedPoolAssignments_SkipsLiveSessionAssignedByAliasHistory(t *testing.T) {
	// Regression: a polecat may have been rebranded (alias rotated) while
	// retaining ownership of work assigned under the prior alias. The
	// previous alias is preserved in Metadata["alias_history"], so
	// orphan-release must consult history before deciding the assignee is
	// dead.
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         "polecat-gc-vi6hhp",
			"alias":                "rictus",
			"alias_history":        "nux",
			"template":             "worker",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "claimed pool work",
		Assignee: "nux",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		[]beads.Bead{sessionBead},
		[]beads.Bead{work},
		[]beads.Store{store},
		nil,
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none — live polecat owns work via prior alias", released)
	}
}

func TestReleaseOrphanedPoolAssignments_SkipsLiveSessionByAliasViaLiveList(t *testing.T) {
	// Even without an upstream session snapshot, the fallback live-list
	// path must recognize alias-owned work. This covers ticks where the
	// session snapshot is missing or stale (e.g. partial reads).
	store := beads.NewMemStore()
	_, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         "polecat-gc-vi6hhp",
			"alias":                "nux",
			"template":             "worker",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "claimed pool work",
		Assignee: "nux",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		nil,
		[]beads.Bead{work},
		[]beads.Store{store},
		nil,
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none — live-list fallback must resolve alias", released)
	}
}

func TestReleaseOrphanedPoolAssignments_SkipsWorkReassignedAfterCandidateSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "claimed pool work",
		Assignee: "worker-old",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	candidate, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload candidate work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Assignee: stringPtr("worker-new")}); err != nil {
		t.Fatalf("Reassign work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		nil,
		[]beads.Bead{candidate},
		[]beads.Store{store},
		nil,
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none for reassigned work", released)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", got.Status)
	}
	if got.Assignee != "worker-new" {
		t.Fatalf("assignee = %q, want worker-new", got.Assignee)
	}
}

func TestReleaseOrphanedPoolAssignments_ReopensUnassignedInProgressPoolWork(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "stranded pool work",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}
	if work.Assignee != "" {
		t.Fatalf("test setup assignee = %q, want empty", work.Assignee)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		nil,
		[]beads.Bead{work},
		nil,
		nil,
		nil,
	)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s]", released, work.ID)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("status = %q, want open", got.Status)
	}
	if got.Assignee != "" {
		t.Fatalf("assignee = %q, want empty", got.Assignee)
	}
}

func TestCollectAssignedWorkBeadsIncludesUnassignedInProgressPoolWorkForRecovery(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "stranded pool work",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}

	found, stores, _, partial := collectAssignedWorkBeadsWithStores(
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		store,
		nil,
		nil,
		nil,
	)
	if partial {
		t.Fatal("collectAssignedWorkBeadsWithStores reported partial results")
	}
	if len(found) != 1 || found[0].ID != work.ID {
		t.Fatalf("found = %#v, want stranded work %s", found, work.ID)
	}
	if len(stores) != 1 || stores[0] != store {
		t.Fatalf("stores = %#v, want owner store", stores)
	}
}

func TestReleaseOrphanedPoolAssignments_UpdatesRigStoreFallback(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	work, err := rigStore.Create(beads.Bead{
		Title:    "orphaned rig pool work",
		Assignee: "worker-dead",
		Metadata: map[string]string{"gc.routed_to": "rig/worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := rigStore.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		cityStore,
		&config.City{
			Rigs:   []config.Rig{{Name: "rig", Prefix: "ga"}},
			Agents: []config.Agent{{Name: "worker", Dir: "rig", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		},
		"",
		nil,
		[]beads.Bead{work},
		nil,
		nil,
		map[string]beads.Store{"rig": rigStore},
	)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s]", released, work.ID)
	}

	got, err := rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("Get rig work bead: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("rig status = %q, want open", got.Status)
	}
	if got.Assignee != "" {
		t.Fatalf("rig assignee = %q, want empty", got.Assignee)
	}
	if _, err := cityStore.Get(work.ID); err == nil {
		t.Fatalf("city store unexpectedly contains rig work bead %s", work.ID)
	}
}

func TestReleaseOrphanedPoolAssignments_ReopensRigStoreMissingPoolAssignee(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	citySession, err := cityStore.Create(beads.Bead{
		Title:    "worker session",
		Type:     sessionBeadType,
		Status:   "open",
		Assignee: "worker-live",
		Metadata: map[string]string{
			"session_name":         "worker-dead",
			"template":             "worker",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create city session bead: %v", err)
	}
	work, err := rigStore.Create(beads.Bead{
		Title:    "orphaned rig pool work",
		Assignee: "worker-dead",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create rig work bead: %v", err)
	}
	if err := rigStore.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set rig work status: %v", err)
	}
	work, err = rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload rig work bead: %v", err)
	}
	if citySession.ID != work.ID {
		t.Fatalf("test setup expected overlapping city/rig IDs, got city %q rig %q", citySession.ID, work.ID)
	}

	released := releaseOrphanedPoolAssignments(
		cityStore,
		&config.City{
			Rigs:   []config.Rig{{Name: "repo"}},
			Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		},
		"",
		nil,
		[]beads.Bead{work},
		[]beads.Store{rigStore},
		nil,
		nil,
	)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s]", released, work.ID)
	}

	got, err := rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("Get rig work bead: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("status = %q, want open", got.Status)
	}
	if got.Assignee != "" {
		t.Fatalf("assignee = %q, want empty", got.Assignee)
	}
	gotSession, err := cityStore.Get(citySession.ID)
	if err != nil {
		t.Fatalf("Get city session bead: %v", err)
	}
	if gotSession.Type != sessionBeadType {
		t.Fatalf("city session type = %q, want %q", gotSession.Type, sessionBeadType)
	}
	if gotSession.Assignee != "worker-live" {
		t.Fatalf("city session assignee = %q, want worker-live", gotSession.Assignee)
	}
	if gotSession.Metadata["session_name"] != "worker-dead" ||
		gotSession.Metadata["template"] != "worker" ||
		gotSession.Metadata[poolManagedMetadataKey] != boolMetadata(true) {
		t.Fatalf("city session metadata changed: %#v", gotSession.Metadata)
	}
}

func TestReleaseOrphanedPoolAssignments_ReopensCrossStoreIDCollisions(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	cityWork, err := cityStore.Create(beads.Bead{
		Title:    "orphaned city pool work",
		Assignee: "worker-city-dead",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create city work bead: %v", err)
	}
	if err := cityStore.Update(cityWork.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set city work status: %v", err)
	}
	cityWork, err = cityStore.Get(cityWork.ID)
	if err != nil {
		t.Fatalf("Reload city work bead: %v", err)
	}
	rigWork, err := rigStore.Create(beads.Bead{
		Title:    "orphaned rig pool work",
		Assignee: "worker-rig-dead",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create rig work bead: %v", err)
	}
	if err := rigStore.Update(rigWork.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set rig work status: %v", err)
	}
	rigWork, err = rigStore.Get(rigWork.ID)
	if err != nil {
		t.Fatalf("Reload rig work bead: %v", err)
	}
	if cityWork.ID != rigWork.ID {
		t.Fatalf("test setup expected overlapping city/rig IDs, got city %q rig %q", cityWork.ID, rigWork.ID)
	}

	released := releaseOrphanedPoolAssignments(
		cityStore,
		&config.City{
			Rigs:   []config.Rig{{Name: "repo"}},
			Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		},
		"",
		nil,
		[]beads.Bead{cityWork, rigWork},
		[]beads.Store{cityStore, rigStore},
		nil,
		nil,
	)
	if len(released) != 2 || released[0].ID != cityWork.ID || released[1].ID != rigWork.ID {
		t.Fatalf("released = %v, want [%s %s]", released, cityWork.ID, rigWork.ID)
	}
	gotCity, err := cityStore.Get(cityWork.ID)
	if err != nil {
		t.Fatalf("Get city work bead: %v", err)
	}
	if gotCity.Status != "open" || gotCity.Assignee != "" {
		t.Fatalf("city work = status %q assignee %q, want open/unassigned", gotCity.Status, gotCity.Assignee)
	}
	gotRig, err := rigStore.Get(rigWork.ID)
	if err != nil {
		t.Fatalf("Get rig work bead: %v", err)
	}
	if gotRig.Status != "open" || gotRig.Assignee != "" {
		t.Fatalf("rig work = status %q assignee %q, want open/unassigned", gotRig.Status, gotRig.Assignee)
	}
}

func TestReleaseOrphanedPoolAssignments_SkipsStoreAwareEntryWithoutOwnerStore(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	work, err := rigStore.Create(beads.Bead{
		Title:    "orphaned rig pool work",
		Assignee: "worker-dead",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create rig work bead: %v", err)
	}
	if err := rigStore.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set rig work status: %v", err)
	}
	work, err = rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload rig work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		cityStore,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		nil,
		[]beads.Bead{work},
		[]beads.Store{nil},
		nil,
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none without owner store", released)
	}
	got, err := rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("Get rig work bead: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "worker-dead" {
		t.Fatalf("rig work = status %q assignee %q, want unchanged in_progress/worker-dead", got.Status, got.Assignee)
	}
}

func TestReleaseOrphanedPoolAssignments_KeepsOpenSessionOwnership(t *testing.T) {
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   "session",
		Status: "open",
		Metadata: map[string]string{
			"session_name":         "worker-live",
			"template":             "worker",
			"agent_name":           "worker",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "live pool work",
		Assignee: "worker-live",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		[]beads.Bead{session},
		[]beads.Bead{work},
		nil,
		nil,
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none", released)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", got.Status)
	}
	if got.Assignee != "worker-live" {
		t.Fatalf("assignee = %q, want worker-live", got.Assignee)
	}
}

func TestReleaseOrphanedPoolAssignments_ReleasesRigWorkAssignedToUnreachableOpenSession(t *testing.T) {
	cityPath := t.TempDir()
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	session, err := cityStore.Create(beads.Bead{
		Title:  "city worker",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name":         "worker-live",
			"template":             "worker",
			"agent_name":           "worker",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create city session bead: %v", err)
	}
	work, err := rigStore.Create(beads.Bead{
		Title:    "misassigned rig pool work",
		Assignee: "worker-live",
		Metadata: map[string]string{"gc.routed_to": "repo/worker"},
	})
	if err != nil {
		t.Fatalf("Create rig work bead: %v", err)
	}
	if err := rigStore.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set rig work status: %v", err)
	}
	work, err = rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload rig work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		cityStore,
		&config.City{
			Rigs: []config.Rig{{Name: "repo", Path: t.TempDir()}},
			Agents: []config.Agent{
				{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
				{Name: "worker", Dir: "repo", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
			},
		},
		cityPath,
		[]beads.Bead{session},
		[]beads.Bead{work},
		[]beads.Store{rigStore},
		[]string{"repo"},
		map[string]beads.Store{"repo": rigStore},
	)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s]", released, work.ID)
	}

	got, err := rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("Get rig work bead: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("rig work = status %q assignee %q, want open/unassigned", got.Status, got.Assignee)
	}
	gotSession, err := cityStore.Get(session.ID)
	if err != nil {
		t.Fatalf("Get city session bead: %v", err)
	}
	if gotSession.Status != "open" || gotSession.Metadata["session_name"] != "worker-live" {
		t.Fatalf("city session changed: status=%q metadata=%#v", gotSession.Status, gotSession.Metadata)
	}
}

func TestStoreForPoolAssignment_UsesConfiguredHyphenatedIDPrefix(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	cfg := &config.City{
		Rigs: []config.Rig{{
			Name:   "pieces",
			Prefix: "Pieces-Annotator",
			Path:   t.TempDir(),
		}},
	}
	work := beads.Bead{
		ID:       "pieces-annotator-x8o",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	}

	got := storeForPoolAssignment(cfg, cityStore, map[string]beads.Store{"pieces": rigStore}, work)
	if got != rigStore {
		t.Fatalf("storeForPoolAssignment() = %p, want rig store %p", got, rigStore)
	}
}

func TestReleaseOrphanedPoolAssignments_KeepsSameStoreScopedOpenSessionOwnership(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name":         "worker-live",
			"template":             "worker",
			"agent_name":           "worker",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "live pool work",
		Assignee: "worker-live",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		cityPath,
		[]beads.Bead{session},
		[]beads.Bead{work},
		[]beads.Store{store},
		[]string{""},
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none", released)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", got.Status)
	}
	if got.Assignee != "worker-live" {
		t.Fatalf("assignee = %q, want worker-live", got.Assignee)
	}
}

func TestReleaseOrphanedPoolAssignments_ReopensStaleDirectAssigneeForNamedBackedTemplate(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "stale direct-session work",
		Assignee: "mc-dead",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}

	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{
			Name:     "reviewer",
			Template: "worker",
			Mode:     "on_demand",
		}},
		ResolvedWorkspaceName: "test-city",
	}

	released := releaseOrphanedPoolAssignments(store, cfg, "", nil, []beads.Bead{work}, nil, nil, nil)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s]", released, work.ID)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("status = %q, want open", got.Status)
	}
	if got.Assignee != "" {
		t.Fatalf("assignee = %q, want empty", got.Assignee)
	}
}

func TestReleaseOrphanedPoolAssignments_PreservesCanonicalNamedIdentity(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "named owner work",
		Assignee: "reviewer",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}

	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{
			Name:     "reviewer",
			Template: "worker",
			Mode:     "on_demand",
		}},
		ResolvedWorkspaceName: "test-city",
	}

	released := releaseOrphanedPoolAssignments(store, cfg, "", nil, []beads.Bead{work}, nil, nil, nil)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none", released)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", got.Status)
	}
	if got.Assignee != "reviewer" {
		t.Fatalf("assignee = %q, want reviewer", got.Assignee)
	}
}

func TestReleaseOrphanedPoolAssignments_ReleasesNamedIdentityForUnreachableStore(t *testing.T) {
	cityPath := t.TempDir()
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	work, err := rigStore.Create(beads.Bead{
		Title:    "misassigned named work",
		Assignee: "reviewer",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create rig work bead: %v", err)
	}
	if err := rigStore.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set rig work status: %v", err)
	}
	work, err = rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload rig work bead: %v", err)
	}

	cfg := &config.City{
		Rigs:   []config.Rig{{Name: "repo", Path: t.TempDir()}},
		Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{
			Name:     "reviewer",
			Template: "worker",
			Mode:     "on_demand",
		}},
		ResolvedWorkspaceName: "test-city",
	}

	released := releaseOrphanedPoolAssignments(
		cityStore,
		cfg,
		cityPath,
		nil,
		[]beads.Bead{work},
		[]beads.Store{rigStore},
		[]string{"repo"},
		map[string]beads.Store{"repo": rigStore},
	)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s]", released, work.ID)
	}

	got, err := rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("Get rig work bead: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("rig work = status %q assignee %q, want open/unassigned", got.Status, got.Assignee)
	}
}

func TestReleaseOrphanedPoolAssignments_PreservesNamedIdentityForSameStore(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "named owner work",
		Assignee: "reviewer",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}

	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{
			Name:     "reviewer",
			Template: "worker",
			Mode:     "on_demand",
		}},
		ResolvedWorkspaceName: "test-city",
	}

	released := releaseOrphanedPoolAssignments(
		store,
		cfg,
		cityPath,
		nil,
		[]beads.Bead{work},
		[]beads.Store{store},
		[]string{""},
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none", released)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", got.Status)
	}
	if got.Assignee != "reviewer" {
		t.Fatalf("assignee = %q, want reviewer", got.Assignee)
	}
}

// TestReleaseOrphanedPoolAssignments_CapturesPrevAssigneeAndRoute locks in that
// the reopen record carries the stale claimant (PrevAssignee) and the pool
// route so the pool.assignment_reopened event can report who lost the bead.
func TestReleaseOrphanedPoolAssignments_CapturesPrevAssigneeAndRoute(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "stale-claimed pool work",
		Assignee: "worker-dead",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		nil,
		[]beads.Bead{work},
		nil,
		nil,
		nil,
	)
	if len(released) != 1 {
		t.Fatalf("released = %v, want 1 entry", released)
	}
	if released[0].ID != work.ID {
		t.Fatalf("released[0].ID = %q, want %q", released[0].ID, work.ID)
	}
	if released[0].PrevAssignee != "worker-dead" {
		t.Fatalf("released[0].PrevAssignee = %q, want %q", released[0].PrevAssignee, "worker-dead")
	}
	if released[0].Route != "worker" {
		t.Fatalf("released[0].Route = %q, want %q", released[0].Route, "worker")
	}
}

// TestPoolAssignmentReopenedEvent asserts the pure event builder produces a
// typed pool.assignment_reopened envelope + payload with the right fields.
func TestPoolAssignmentReopenedEvent(t *testing.T) {
	ev := poolAssignmentReopenedEvent(releasedPoolAssignment{
		ID:           "ga-42",
		PrevAssignee: "gastown__polecat-pe-dead",
		Route:        "gascity/gastown.polecat",
	})
	if ev.Type != events.PoolAssignmentReopened {
		t.Fatalf("Type = %q, want %q", ev.Type, events.PoolAssignmentReopened)
	}
	if ev.Actor != "gc" {
		t.Fatalf("Actor = %q, want gc (mechanism-only signal, no role name)", ev.Actor)
	}
	if ev.Subject != "ga-42" {
		t.Fatalf("Subject = %q, want ga-42", ev.Subject)
	}
	var got api.PoolAssignmentReopenedPayload
	if err := json.Unmarshal(ev.Payload, &got); err != nil {
		t.Fatalf("Unmarshal payload: %v", err)
	}
	want := api.PoolAssignmentReopenedPayload{
		BeadID:       "ga-42",
		PrevAssignee: "gastown__polecat-pe-dead",
		Template:     "gascity/gastown.polecat",
		Reason:       poolAssignmentReopenedReason,
	}
	if got != want {
		t.Fatalf("payload = %+v, want %+v", got, want)
	}
}

// TestPoolAssignmentReopenedEvent_EmptyPrevAssignee covers the unassigned
// in-progress recovery sub-case: prev_assignee is omitted from the wire form.
func TestPoolAssignmentReopenedEvent_EmptyPrevAssignee(t *testing.T) {
	ev := poolAssignmentReopenedEvent(releasedPoolAssignment{ID: "ga-7", Route: "worker"})
	if !strings.Contains(string(ev.Payload), `"bead_id":"ga-7"`) {
		t.Fatalf("payload %s missing bead_id", ev.Payload)
	}
	if strings.Contains(string(ev.Payload), "prev_assignee") {
		t.Fatalf("payload %s should omit empty prev_assignee", ev.Payload)
	}
}

// TestRecordReleasedPoolAssignments_EmitsPerBeadAndLogs asserts the shared sink
// logs each reopen to stderr and emits exactly one typed event per bead.
func TestRecordReleasedPoolAssignments_EmitsPerBeadAndLogs(t *testing.T) {
	fake := events.NewFake()
	var stderr bytes.Buffer
	released := []releasedPoolAssignment{
		{ID: "ga-1", PrevAssignee: "sess-a", Route: "worker"},
		{ID: "ga-2", PrevAssignee: "", Route: "worker"},
	}

	recordReleasedPoolAssignments(fake, &stderr, released)

	var got []events.Event
	for _, e := range fake.Events {
		if e.Type == events.PoolAssignmentReopened {
			got = append(got, e)
		}
	}
	if len(got) != 2 {
		t.Fatalf("emitted %d pool.assignment_reopened events, want 2", len(got))
	}
	if got[0].Subject != "ga-1" || got[1].Subject != "ga-2" {
		t.Fatalf("subjects = %q,%q, want ga-1,ga-2", got[0].Subject, got[1].Subject)
	}
	for _, id := range []string{"ga-1", "ga-2"} {
		if !strings.Contains(stderr.String(), "released orphaned pool work: "+id) {
			t.Fatalf("stderr %q missing log line for %s", stderr.String(), id)
		}
	}
}

// TestRecordReleasedPoolAssignments_NilRecorderStillLogs guards the callers
// that may lack an event bus (e.g. a one-shot start with events.Discard): a
// nil recorder must skip emission without panicking and still log to stderr.
func TestRecordReleasedPoolAssignments_NilRecorderStillLogs(t *testing.T) {
	var stderr bytes.Buffer
	recordReleasedPoolAssignments(nil, &stderr, []releasedPoolAssignment{{ID: "ga-9", Route: "worker"}})
	if !strings.Contains(stderr.String(), "released orphaned pool work: ga-9") {
		t.Fatalf("stderr %q missing log line", stderr.String())
	}
}

// releaseGetStub is a minimal beads.Store that returns a fixed bead (or error)
// from Get, so verifyReleasedPoolAssignment can be exercised against a
// controlled read-back. Only Get is called; the embedded nil Store makes the
// remaining interface methods compile but they must never run.
type releaseGetStub struct {
	beads.Store
	bead beads.Bead
	err  error
}

func (s releaseGetStub) Get(string) (beads.Bead, error) { return s.bead, s.err }

// reclaimRaceStore backs Update against a real store but rewrites the Get
// read-back to a foreign assignee, simulating a concurrent re-claim that lands
// in the window after releaseOrphanedPoolAssignment's release write.
type reclaimRaceStore struct {
	beads.Store
	reclaimAssignee string
}

func (s reclaimRaceStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	if err != nil {
		return b, err
	}
	b.Assignee = s.reclaimAssignee
	return b, nil
}

// withCapturedLog captures everything written to the standard logger while fn
// runs and returns it, restoring the prior writer and flags afterward. Callers
// must not add t.Parallel(): it mutates process-global logger state.
func withCapturedLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	}()
	fn()
	return buf.String()
}

// TestVerifyReleasedPoolAssignment_LogsForeignReclaim asserts the verify-after
// read logs a loud RELEASE-RACE line only when the bead reads back with a
// foreign, non-empty assignee (a concurrent claim that raced the release
// write). An empty read-back (uncontended release) or a read-back equal to the
// expected assignee stays quiet.
func TestVerifyReleasedPoolAssignment_LogsForeignReclaim(t *testing.T) {
	tests := []struct {
		name     string
		observed string
		expected string
		wantRace bool
	}{
		{name: "foreign reclaim after release", observed: "worker-fresh", expected: "", wantRace: true},
		{name: "uncontended release reads back empty", observed: "", expected: "", wantRace: false},
		{name: "read-back equals expected stays quiet", observed: "worker-held", expected: "worker-held", wantRace: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := releaseGetStub{bead: beads.Bead{ID: "ga-42", Assignee: tc.observed}}
			out := withCapturedLog(t, func() {
				verifyReleasedPoolAssignment(store, "ga-42", tc.expected)
			})
			gotRace := strings.Contains(out, "RELEASE RACE on ga-42")
			if gotRace != tc.wantRace {
				t.Fatalf("RELEASE RACE logged = %v, want %v (log: %q)", gotRace, tc.wantRace, out)
			}
			if tc.wantRace && !strings.Contains(out, tc.observed) {
				t.Fatalf("race log %q missing observed assignee %q", out, tc.observed)
			}
		})
	}
}

// TestVerifyReleasedPoolAssignment_LogsReadFailure asserts a failed verify-after
// read is surfaced (not swallowed) and does not masquerade as a release race.
func TestVerifyReleasedPoolAssignment_LogsReadFailure(t *testing.T) {
	store := releaseGetStub{err: errors.New("boom")}
	out := withCapturedLog(t, func() {
		verifyReleasedPoolAssignment(store, "ga-42", "")
	})
	if !strings.Contains(out, `verify-after read failed for "ga-42"`) {
		t.Fatalf("log %q missing read-failure diagnostic", out)
	}
	if strings.Contains(out, "RELEASE RACE") {
		t.Fatalf("read failure must not be logged as a release race: %q", out)
	}
}

// TestReleaseOrphanedPoolAssignment_ObservesRacedReclaim asserts the release
// helper wires the verify-after read: a foreign read-back emits the RELEASE-RACE
// log while the release still reports success, and an uncontended release stays
// quiet and leaves the bead open+unassigned. Observability only — the release
// decision (return value) is unchanged.
func TestReleaseOrphanedPoolAssignment_ObservesRacedReclaim(t *testing.T) {
	newInProgressWork := func(t *testing.T, mem beads.Store) beads.Bead {
		t.Helper()
		work, err := mem.Create(beads.Bead{Title: "orphaned pool work", Assignee: "worker-dead"})
		if err != nil {
			t.Fatalf("Create work bead: %v", err)
		}
		if err := mem.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
			t.Fatalf("Set work status: %v", err)
		}
		return work
	}

	t.Run("raced reclaim is observed", func(t *testing.T) {
		mem := beads.NewMemStore()
		work := newInProgressWork(t, mem)
		store := reclaimRaceStore{Store: mem, reclaimAssignee: "worker-fresh"}

		var ok bool
		out := withCapturedLog(t, func() {
			ok = releaseOrphanedPoolAssignment(store, work.ID)
		})
		if !ok {
			t.Fatalf("releaseOrphanedPoolAssignment = false, want true (release decision must be unchanged)")
		}
		if !strings.Contains(out, "RELEASE RACE on "+work.ID) || !strings.Contains(out, "worker-fresh") {
			t.Fatalf("expected RELEASE RACE log naming worker-fresh, got %q", out)
		}
	})

	t.Run("uncontended release stays quiet", func(t *testing.T) {
		mem := beads.NewMemStore()
		work := newInProgressWork(t, mem)

		var ok bool
		out := withCapturedLog(t, func() {
			ok = releaseOrphanedPoolAssignment(mem, work.ID)
		})
		if !ok {
			t.Fatalf("releaseOrphanedPoolAssignment = false, want true")
		}
		if strings.Contains(out, "RELEASE RACE") {
			t.Fatalf("uncontended release must stay quiet, got %q", out)
		}
		got, err := mem.Get(work.ID)
		if err != nil {
			t.Fatalf("Get work bead: %v", err)
		}
		if got.Assignee != "" || got.Status != "open" {
			t.Fatalf("post-release bead = {assignee:%q status:%q}, want {assignee:'' status:open}", got.Assignee, got.Status)
		}
	})
}
