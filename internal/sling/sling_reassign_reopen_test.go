package sling

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// orderClaimedPoolHandoffSetup builds a sling against a worker pool for a bead
// an order has already claimed (status=in_progress, assignee=order:<name>),
// reproducing the gastownhall/gascity#3231 starting state. The agent is a
// multi-session pool in a rig so the bead is routed to the pool's claim queue
// rather than a single named session. MemStore.Create forces status=open, so
// the in_progress/assignee state is applied via a follow-up Update.
func orderClaimedPoolHandoffSetup(t *testing.T) (SlingOpts, SlingDeps, beads.Bead) {
	t.Helper()
	runner := newFakeRunner()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{
			{Name: "myrig", Path: "/myrig", Prefix: "gc"},
		},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(2)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	bead, err := deps.Store.Create(beads.Bead{Title: "hotspot work", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	inProgress, orderActor := "in_progress", "order:mol-dog-jsonl"
	if err := deps.Store.Update(bead.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &orderActor}); err != nil {
		t.Fatalf("Update to order-claimed state: %v", err)
	}
	opts := SlingOpts{Target: a, BeadOrFormula: bead.ID, NoFormula: true, Reassign: true}
	return opts, deps, bead
}

// TestDoSling_Reassign_ReopensOrderClaimedBead is the regression test for
// gastownhall/gascity#3231. An order runs `bd update --claim` on a bead
// (status=in_progress, assignee=order:<name>) and then slings it to a worker
// pool with --reassign. Clearing the assignee alone is not enough: the bead
// stays in_progress, and the Ready filter (which requires status=open) filters
// it out, so no pool worker ever claims it — "work looks in progress, but no
// polecat actually owns it." --reassign must reopen the bead so the target
// pool can claim it.
func TestDoSling_Reassign_ReopensOrderClaimedBead(t *testing.T) {
	opts, deps, bead := orderClaimedPoolHandoffSetup(t)
	if _, err := DoSling(opts, deps, nil); err != nil {
		t.Fatalf("DoSling --reassign: %v", err)
	}
	got, err := deps.Store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", bead.ID, err)
	}
	if got.Assignee != "" {
		t.Errorf("Assignee = %q, want empty after --reassign (order actor must not retain pool work)", got.Assignee)
	}
	if got.Status != "open" {
		t.Errorf("Status = %q, want open after --reassign (an in_progress bead handed to a pool must be reopened so it is claimable)", got.Status)
	}
}

// TestDoSling_Reassign_PreservesNonInProgressStatus guards the reopen from
// over-reaching: --reassign only reopens in_progress beads. A bead in another
// status (here, blocked) keeps its status; only the assignee is cleared.
func TestDoSling_Reassign_PreservesNonInProgressStatus(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs:      []config.Rig{{Name: "myrig", Path: "/myrig", Prefix: "gc"}},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(2)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	bead, err := deps.Store.Create(beads.Bead{Title: "blocked work", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	blocked, orderActor := "blocked", "order:mol-dog-jsonl"
	if err := deps.Store.Update(bead.ID, beads.UpdateOpts{Status: &blocked, Assignee: &orderActor}); err != nil {
		t.Fatalf("Update to blocked state: %v", err)
	}
	opts := SlingOpts{Target: a, BeadOrFormula: bead.ID, NoFormula: true, Reassign: true}
	if _, err := DoSling(opts, deps, nil); err != nil {
		t.Fatalf("DoSling --reassign: %v", err)
	}
	got, err := deps.Store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", bead.ID, err)
	}
	if got.Assignee != "" {
		t.Errorf("Assignee = %q, want empty after --reassign", got.Assignee)
	}
	if got.Status != "blocked" {
		t.Errorf("Status = %q, want blocked (reopen must only apply to in_progress beads)", got.Status)
	}
}

// TestReopenForReassign_InProgressEmptyAssignee is a direct unit test of the
// helper: an in_progress bead whose assignee is already empty must still be
// reopened. The assignee-clear and the status-reset are independent — the
// reopen must not be gated behind a non-empty assignee.
func TestReopenForReassign_InProgressEmptyAssignee(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{Title: "task", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(bead.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("Update to in_progress: %v", err)
	}
	if err := reopenForReassign(bead.ID, SlingDeps{Store: store}); err != nil {
		t.Fatalf("reopenForReassign: %v", err)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", bead.ID, err)
	}
	if got.Status != "open" {
		t.Errorf("Status = %q, want open (in_progress bead must be reopened even with an empty assignee)", got.Status)
	}
}

// TestReopenForReassign_OpenUnassignedNoWrite guards the no-op path: a bead
// that is already open and unassigned needs no store write, so the helper must
// return without an Update. A recording store fails the test if Update fires.
func TestReopenForReassign_OpenUnassignedNoWrite(t *testing.T) {
	base := beads.NewMemStore()
	bead, err := base.Create(beads.Bead{Title: "task", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec := &updateRecordingStore{Store: base}
	if err := reopenForReassign(bead.ID, SlingDeps{Store: rec}); err != nil {
		t.Fatalf("reopenForReassign: %v", err)
	}
	if rec.updates != 0 {
		t.Errorf("Update called %d times, want 0 (open+unassigned bead must not trigger a store write)", rec.updates)
	}
}

// updateRecordingStore counts Update calls to assert the no-spurious-write
// contract of reopenForReassign.
type updateRecordingStore struct {
	beads.Store
	updates int
}

func (s *updateRecordingStore) Update(id string, opts beads.UpdateOpts) error {
	s.updates++
	return s.Store.Update(id, opts)
}

// TestReopenForReassign_RigStore: reopenForReassign reopens and clears a
// rig-prefixed bead whose record lives in a source-workflow (rig) store rather
// than the city primary store. The primary store (cityStore) does not hold the
// bead, so the multi-store sweep must find it in the rig store and apply the
// reopen there. Direct unit test of the SourceWorkflowStores fallback — the
// gastownhall/gascity#3408 multi-store gap on the #3231 reopen path.
func TestReopenForReassign_RigStore(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	bead, err := rigStore.Create(beads.Bead{Title: "task", Type: "task"})
	if err != nil {
		t.Fatalf("rig Create: %v", err)
	}
	inProgress, actor := "in_progress", "order:mol-dog-jsonl"
	if err := rigStore.Update(bead.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &actor}); err != nil {
		t.Fatalf("rig Update to order-claimed state: %v", err)
	}
	deps := SlingDeps{
		Store: cityStore,
		SourceWorkflowStores: func() ([]SourceWorkflowStore, error) {
			return []SourceWorkflowStore{{Store: rigStore, StoreRef: "rig:myrig"}}, nil
		},
	}
	if err := reopenForReassign(bead.ID, deps); err != nil {
		t.Fatalf("reopenForReassign: %v", err)
	}
	got, err := rigStore.Get(bead.ID)
	if err != nil {
		t.Fatalf("rig Get: %v", err)
	}
	if got.Assignee != "" {
		t.Errorf("Assignee = %q, want empty after reopen in rig store", got.Assignee)
	}
	if got.Status != "open" {
		t.Errorf("Status = %q, want open (an in_progress rig-store bead must be reopened via the source-workflow sweep)", got.Status)
	}
}

// TestReopenForReassign_PrimaryStoreReadError: a non-ErrNotFound failure from
// the city primary store must abort the reopen with a contextual error rather
// than falling through to the source-workflow sweep. A real read failure under
// --force --reassign would otherwise be treated like a miss, so routing could
// proceed with the bead un-reopened (or a same-ID bead reopened in a different
// store). Regression for the "Don't Swallow Errors" contract.
func TestReopenForReassign_PrimaryStoreReadError(t *testing.T) {
	rigStore := beads.NewMemStore()
	bead, err := rigStore.Create(beads.Bead{Title: "task", Type: "task", Assignee: "human"})
	if err != nil {
		t.Fatalf("rig Create: %v", err)
	}
	sourceSwept := false
	deps := SlingDeps{
		Store: &getErrStore{Store: beads.NewMemStore(), err: fmt.Errorf("backend unavailable")},
		SourceWorkflowStores: func() ([]SourceWorkflowStore, error) {
			sourceSwept = true
			return []SourceWorkflowStore{{Store: rigStore, StoreRef: "rig:myrig"}}, nil
		},
	}
	err = reopenForReassign(bead.ID, deps)
	if err == nil {
		t.Fatal("reopenForReassign error = nil, want primary read failure")
	}
	if !strings.Contains(err.Error(), "backend unavailable") {
		t.Fatalf("error = %q, want wrapped primary read failure", err)
	}
	if strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %q, want store failure, not a not-found miss", err)
	}
	if sourceSwept {
		t.Fatal("source-workflow stores swept after a primary read failure; want abort before fallback")
	}
	got, err := rigStore.Get(bead.ID)
	if err != nil {
		t.Fatalf("rig Get: %v", err)
	}
	if got.Assignee != "human" {
		t.Fatalf("Assignee = %q, want unchanged (reopen must not run after a primary read failure)", got.Assignee)
	}
}

// TestReopenForReassign_SourceStoreReadError: a non-ErrNotFound failure while
// reading a source-workflow store during the rig-store sweep aborts the reopen
// with a store-ref-qualified error instead of silently skipping the store,
// which would leave the bead in_progress/assigned and pool-invisible under
// partial store failure.
func TestReopenForReassign_SourceStoreReadError(t *testing.T) {
	deps := SlingDeps{
		Store: beads.NewMemStore(), // bead is absent here, so the sweep runs
		SourceWorkflowStores: func() ([]SourceWorkflowStore, error) {
			return []SourceWorkflowStore{
				{Store: &getErrStore{Store: beads.NewMemStore(), err: fmt.Errorf("rig store unreadable")}, StoreRef: "rig:myrig"},
			}, nil
		},
	}
	err := reopenForReassign("gc-123", deps)
	if err == nil {
		t.Fatal("reopenForReassign error = nil, want source-store read failure")
	}
	if !strings.Contains(err.Error(), "rig store unreadable") {
		t.Fatalf("error = %q, want wrapped source-store read failure", err)
	}
	if !strings.Contains(err.Error(), "rig:myrig") {
		t.Fatalf("error = %q, want store-ref context to localize the failing store", err)
	}
}

// TestReopenForReassign_SourceStoreListError: a failure from the
// SourceWorkflowStores lister itself — the callback returning an error before
// any store can be scanned, distinct from a per-store Get failure — aborts the
// reopen with a bead-qualified error instead of silently no-op'ing. Fail-loud
// guard for the --reassign contract: if the source-workflow stores cannot even
// be listed after a primary-store miss, routing must not proceed as though the
// bead were absent everywhere and leave it routed-but-unclaimable.
func TestReopenForReassign_SourceStoreListError(t *testing.T) {
	deps := SlingDeps{
		Store: beads.NewMemStore(), // bead is absent here (ErrNotFound), so the sweep runs
		SourceWorkflowStores: func() ([]SourceWorkflowStore, error) {
			return nil, fmt.Errorf("stores unavailable")
		},
	}
	err := reopenForReassign("gc-456", deps)
	if err == nil {
		t.Fatal("reopenForReassign error = nil, want source-workflow store listing failure")
	}
	if !strings.Contains(err.Error(), "listing source-workflow stores") {
		t.Fatalf("error = %q, want wrapped source-workflow store listing failure", err)
	}
	if !strings.Contains(err.Error(), "stores unavailable") {
		t.Fatalf("error = %q, want underlying lister error preserved", err)
	}
	if !strings.Contains(err.Error(), "gc-456") {
		t.Fatalf("error = %q, want bead ID context to localize the failed reopen", err)
	}
}

// TestReopenForReassign_NilPrimaryStore: with no city primary store, the reopen
// still sweeps the source-workflow stores and clears the assignee where the
// bead lives, matching the multi-store behavior of sourceWorkflowRootByID. A
// nil deps.Store must not skip available rig stores.
func TestReopenForReassign_NilPrimaryStore(t *testing.T) {
	rigStore := beads.NewMemStore()
	bead, err := rigStore.Create(beads.Bead{Title: "task", Type: "task", Assignee: "human"})
	if err != nil {
		t.Fatalf("rig Create: %v", err)
	}
	deps := SlingDeps{
		Store: nil,
		SourceWorkflowStores: func() ([]SourceWorkflowStore, error) {
			return []SourceWorkflowStore{{Store: rigStore, StoreRef: "rig:myrig"}}, nil
		},
	}
	if err := reopenForReassign(bead.ID, deps); err != nil {
		t.Fatalf("reopenForReassign: %v", err)
	}
	got, err := rigStore.Get(bead.ID)
	if err != nil {
		t.Fatalf("rig Get: %v", err)
	}
	if got.Assignee != "" {
		t.Fatalf("Assignee = %q, want empty after clearing via the source-workflow sweep with a nil primary store", got.Assignee)
	}
}

// TestDoSling_Reassign_ReopensRigStoreBead: --reassign reopens and clears an
// in_progress rig-prefixed bead whose record lives in the rig store, not the
// city primary store. End-to-end regression for the multi-store gap — the
// reopen previously no-op'd because reopenForReassign only consulted deps.Store,
// so the bead stayed routed+in_progress and invisible to the pool scaler.
func TestDoSling_Reassign_ReopensRigStoreBead(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{
			{Name: "myrig", Path: "/myrig", Prefix: "gc"},
		},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(2)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	rigStore := beads.NewMemStore()
	bead, err := rigStore.Create(beads.Bead{Title: "task", Type: "task"})
	if err != nil {
		t.Fatalf("rig Create: %v", err)
	}
	inProgress, actor := "in_progress", "order:mol-dog-jsonl"
	if err := rigStore.Update(bead.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &actor}); err != nil {
		t.Fatalf("rig Update to order-claimed state: %v", err)
	}
	deps.SourceWorkflowStores = func() ([]SourceWorkflowStore, error) {
		return []SourceWorkflowStore{{Store: rigStore, StoreRef: "rig:myrig"}}, nil
	}
	// Force routing: the bead lives only in the rig store, so the city-store
	// existence validation must be bypassed to reach the reassign reopen.
	opts := SlingOpts{
		Target:        a,
		BeadOrFormula: bead.ID,
		NoFormula:     true,
		NoConvoy:      true,
		Reassign:      true,
		Force:         true,
	}
	if _, err := DoSling(opts, deps, nil); err != nil {
		t.Fatalf("DoSling --reassign on rig-store bead: %v", err)
	}
	got, err := rigStore.Get(bead.ID)
	if err != nil {
		t.Fatalf("rig Get: %v", err)
	}
	if got.Assignee != "" {
		t.Errorf("Assignee = %q, want empty after --reassign", got.Assignee)
	}
	if got.Status != "open" {
		t.Errorf("Status = %q, want open after --reassign (in_progress rig-store bead must be reopened)", got.Status)
	}
}
