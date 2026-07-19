package main

import (
	"fmt"
	"io"
	"log"
	"path"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sling"
)

// sessionBeadAssigneeIdentities returns every identifier under which a work
// bead could be assigned to this session: the session bead ID, session_name,
// configured_named_identity, current alias, and any prior aliases preserved
// in alias_history. Pool polecat aliases (e.g. "nux") are first-class
// assignment identities, so leaving them out of orphan-detection causes
// in-progress work to be reset under a live owner — see the
// SkipsLiveSessionAssignedByAlias regression tests.
func sessionBeadAssigneeIdentities(sb beads.Bead) []string {
	identities := make([]string, 0, 5)
	if id := strings.TrimSpace(sb.ID); id != "" {
		identities = append(identities, id)
	}
	if sn := strings.TrimSpace(sb.Metadata["session_name"]); sn != "" {
		identities = append(identities, sn)
	}
	if ni := strings.TrimSpace(sb.Metadata["configured_named_identity"]); ni != "" {
		identities = append(identities, ni)
	}
	if al := strings.TrimSpace(sb.Metadata["alias"]); al != "" {
		identities = append(identities, al)
	}
	for _, prior := range session.AliasHistory(sb.Metadata) {
		if prior = strings.TrimSpace(prior); prior != "" {
			identities = append(identities, prior)
		}
	}
	return identities
}

type releasedPoolAssignment struct {
	ID    string
	Index int
	// PrevAssignee is the assignee the bead carried before reopen — the stale
	// (dead-session) claimant, or empty when recovering an unassigned
	// in-progress bead. Route is the bead's gc.routed_to pool template. Both
	// feed the pool.assignment_reopened observability event.
	PrevAssignee string
	Route        string
}

// PoolSessionName derives the tmux session name for a pool worker session.
// Format: {basename(template)}-{beadID} (e.g., "claude-mc-xyz").
// Named sessions with an alias use the alias instead.
func PoolSessionName(template, beadID string) string {
	base := path.Base(template)
	return agent.SanitizeQualifiedNameForSession(base) + "-" + beadID
}

// GCSweepSessionBeads closes open session beads that have no remaining
// open/in-progress work beads anywhere — primary store OR any attached
// rig store. Work-bead assignment is verified by a live cross-store
// query inside closeSessionBeadIfUnassigned, so the caller does not
// pass a work snapshot — that pattern was retired to prevent pre-close
// tick snapshots from poisoning close decisions. Returns the IDs of
// session beads that were closed.
func GCSweepSessionBeads(store beads.Store, rigStores map[string]beads.Store, sessionBeads []beads.Bead) []string {
	var closed []string
	for _, sb := range sessionBeads {
		if sb.Status == "closed" {
			continue
		}
		if !closeSessionBeadIfUnassigned(store, rigStores, nil, sb, "gc_swept", time.Now().UTC(), nil) {
			continue
		}
		closed = append(closed, sb.ID)
	}
	return closed
}

// releaseOrphanedPoolAssignmentsWhenSnapshotsComplete skips orphan release
// unless both the assigned-work and open-session snapshots are complete.
func releaseOrphanedPoolAssignmentsWhenSnapshotsComplete(
	store beads.Store,
	cfg *config.City,
	cityPath string,
	openSessionBeads []beads.Bead,
	result DesiredStateResult,
	rigStores map[string]beads.Store,
) []releasedPoolAssignment {
	// Partial input snapshots can make active work look orphaned for this
	// tick only: missing work affects drain decisions, and missing sessions
	// affects assigned-work orphan release.
	if result.snapshotQueryPartial() {
		return nil
	}
	return releaseOrphanedPoolAssignments(store, cfg, cityPath, openSessionBeads, result.AssignedWorkBeads, result.AssignedWorkStores, result.AssignedWorkStoreRefs, rigStores)
}

// releaseOrphanedPoolAssignments reopens active pool-routed work whose
// assignee no longer maps to any open session bead. This also recovers
// pool-routed work left in_progress with no assignee, which cannot be claimed
// again until it is moved back to open.
func releaseOrphanedPoolAssignments(
	store beads.Store,
	cfg *config.City,
	cityPath string,
	openSessionBeads []beads.Bead,
	assignedWorkBeads []beads.Bead,
	assignedWorkStores []beads.Store,
	assignedWorkStoreRefs []string,
	rigStores map[string]beads.Store,
) []releasedPoolAssignment {
	if store == nil || cfg == nil || len(assignedWorkBeads) == 0 {
		return nil
	}
	storeAware := len(assignedWorkStores) > 0
	if storeAware && len(assignedWorkStores) != len(assignedWorkBeads) {
		log.Printf("releaseOrphanedPoolAssignments: assigned work/store length mismatch: work=%d stores=%d", len(assignedWorkBeads), len(assignedWorkStores))
	}
	storeRefAware := len(assignedWorkStoreRefs) == len(assignedWorkBeads)
	if len(assignedWorkStoreRefs) > 0 && !storeRefAware {
		log.Printf("releaseOrphanedPoolAssignments: assigned work/store-ref length mismatch: work=%d storeRefs=%d", len(assignedWorkBeads), len(assignedWorkStoreRefs))
	}

	openIdentifiers := makeOpenSessionStoreRefIndex(cityPath, cfg, openSessionBeads, storeRefAware)
	legacyOpenIdentifiers := make(map[string]struct{}, len(openSessionBeads)*5)
	for _, sb := range openSessionBeads {
		if sb.Status == "closed" {
			continue
		}
		for _, id := range sessionBeadAssigneeIdentities(sb) {
			legacyOpenIdentifiers[id] = struct{}{}
		}
	}

	var released []releasedPoolAssignment
	for i, wb := range assignedWorkBeads {
		if wb.Status != "open" && wb.Status != "in_progress" {
			continue
		}
		assignee := strings.TrimSpace(wb.Assignee)
		template := strings.TrimSpace(wb.Metadata["gc.routed_to"])
		restoredRoute := ""
		if template == "" {
			// Empty-route handoff-orphan recovery (ga-fqy9, upstream
			// 3399cfc0 / #3377). A bead can reach reap with no gc.routed_to
			// — a non-atomic done-handoff, a manual op, a non-pool formula.
			// Pool demand keys on gc.routed_to, so an unrouted bead is
			// invisible to dispatch and the empty-route skip that used to
			// live here stranded it open forever. Recover the route from the
			// reaped owning session bead's own template/agent_name metadata
			// so the reopened bead re-enters pool demand and a fresh worker
			// re-attempts the idempotent handoff. ZFC-safe: the route is the
			// session bead's configured template, never a hardcoded role.
			// Mirrors the ga-kw66 backfill in
			// unclaimWorkAssignedToRetiredSessionBead.
			//
			// Recover only when BOTH routes are empty (upstream's guard): a
			// bead still carrying gc.run_target routes via that mechanism,
			// not this pool path, so restoring gc.routed_to would double-route
			// it. An unassigned bead has no owning session to recover from.
			if assignee == "" || strings.TrimSpace(wb.Metadata["gc.run_target"]) != "" {
				continue
			}
			restoredRoute = orphanedPoolAssignmentFallbackRoute(store, assignee)
			if restoredRoute == "" {
				continue
			}
			template = restoredRoute
		}
		agentCfg := findAgentByTemplate(cfg, template)
		if agentCfg == nil || !agentCfg.SupportsGenericEphemeralSessions() {
			continue
		}
		if assignee == "" {
			if wb.Status != "in_progress" {
				continue
			}
		} else {
			workStoreRef := ""
			if storeRefAware {
				workStoreRef = assignedWorkStoreRefs[i]
			}
			if openSessionOwnsWork(legacyOpenIdentifiers, openIdentifiers, assignee, workStoreRef, storeRefAware) {
				continue
			}
			if assigneePreservesNamedSessionRoute(cfg, cityPath, template, assignee, workStoreRef, storeRefAware) {
				continue
			}
			if liveOpenSessionAssignmentExists(store, assignee) {
				continue
			}
		}

		var ownerStore beads.Store
		if storeAware {
			if i >= len(assignedWorkStores) || assignedWorkStores[i] == nil {
				log.Printf("releaseOrphanedPoolAssignments: missing owner store for assigned work %q at index %d", wb.ID, i)
				continue
			}
			ownerStore = assignedWorkStores[i]
		} else {
			ownerStore = storeForPoolAssignment(cfg, store, rigStores, wb)
			if ownerStore == nil {
				continue
			}
		}
		if !liveWorkAssignmentStillReleasable(ownerStore, wb.ID, assignee) {
			continue
		}
		if !releaseOrphanedPoolAssignmentWithRoute(ownerStore, wb.ID, restoredRoute) {
			continue
		}
		released = append(released, releasedPoolAssignment{ID: wb.ID, Index: i, PrevAssignee: assignee, Route: template})
	}
	return released
}

const unresolvedOpenSessionStoreRef = "\x00unresolved"

func makeOpenSessionStoreRefIndex(cityPath string, cfg *config.City, openSessionBeads []beads.Bead, storeRefAware bool) map[string]map[string]struct{} {
	index := make(map[string]map[string]struct{}, len(openSessionBeads)*5)
	if !storeRefAware {
		return index
	}
	for _, sb := range openSessionBeads {
		if sb.Status == "closed" {
			continue
		}
		storeRef, ok := assignedWorkStoreRefForSession(cityPath, cfg, sb)
		if !ok {
			storeRef = unresolvedOpenSessionStoreRef
		}
		for _, id := range sessionBeadAssigneeIdentities(sb) {
			addOpenSessionStoreRef(index, id, storeRef)
		}
	}
	return index
}

func addOpenSessionStoreRef(index map[string]map[string]struct{}, identifier, storeRef string) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return
	}
	refs := index[identifier]
	if refs == nil {
		refs = make(map[string]struct{}, 1)
		index[identifier] = refs
	}
	refs[storeRef] = struct{}{}
}

func openSessionOwnsWork(legacyIdentifiers map[string]struct{}, scopedIdentifiers map[string]map[string]struct{}, assignee, workStoreRef string, storeRefAware bool) bool {
	if !storeRefAware {
		_, ok := legacyIdentifiers[assignee]
		return ok
	}
	refs := scopedIdentifiers[assignee]
	if refs == nil {
		return false
	}
	if _, ok := refs[unresolvedOpenSessionStoreRef]; ok {
		return true
	}
	_, ok := refs[workStoreRef]
	return ok
}

func storeForPoolAssignment(cfg *config.City, cityStore beads.Store, rigStores map[string]beads.Store, wb beads.Bead) beads.Store {
	if cfg == nil || len(rigStores) == 0 {
		return cityStore
	}
	if routed := strings.TrimSpace(wb.Metadata["gc.routed_to"]); routed != "" {
		if slash := strings.IndexByte(routed, '/'); slash > 0 {
			if store := rigStores[routed[:slash]]; store != nil {
				return store
			}
		}
	}
	idPrefix := sling.BeadPrefixForCity(cfg, wb.ID)
	for _, rig := range cfg.Rigs {
		if strings.EqualFold(idPrefix, rig.EffectivePrefix()) {
			if store := rigStores[rig.Name]; store != nil {
				return store
			}
		}
	}
	return cityStore
}

func isRecoverableUnassignedInProgressPoolWork(cfg *config.City, wb beads.Bead) bool {
	if wb.Status != "in_progress" || strings.TrimSpace(wb.Assignee) != "" {
		return false
	}
	template := strings.TrimSpace(wb.Metadata["gc.routed_to"])
	if template == "" {
		return false
	}
	agentCfg := findAgentByTemplate(cfg, template)
	return agentCfg != nil && agentCfg.SupportsGenericEphemeralSessions()
}

func releaseOrphanedPoolAssignment(store beads.Store, id string) bool {
	return releaseOrphanedPoolAssignmentWithRoute(store, id, "")
}

// releaseOrphanedPoolAssignmentWithRoute reopens an orphaned pool work bead by
// clearing its assignee and resetting status to open. When restoredRoute is
// non-empty (empty-route handoff-orphan recovery), it also backfills
// gc.routed_to so the reopened bead re-enters pool demand; the per-key metadata
// merge in Store.Update leaves unrelated metadata (branch, gc.kind, …)
// untouched. restoredRoute is the empty string for the common
// route-preserving release, which never writes metadata.
func releaseOrphanedPoolAssignmentWithRoute(store beads.Store, id, restoredRoute string) bool {
	if store == nil || id == "" {
		return false
	}
	opts := beads.UpdateOpts{
		Assignee: stringPtr(""),
		Status:   stringPtr("open"),
	}
	if restoredRoute = strings.TrimSpace(restoredRoute); restoredRoute != "" {
		opts.Metadata = map[string]string{"gc.routed_to": restoredRoute}
	}
	if store.Update(id, opts) != nil {
		return false
	}
	verifyReleasedPoolAssignment(store, id, "")
	return true
}

// orphanedPoolAssignmentFallbackRoute recovers the pool route for an
// empty-routed orphan from its owning session bead's own template/agent_name
// metadata. The owning session is, by definition of orphan release, no longer
// live, so this looks up the session bead regardless of status — a reaped pool
// worker's bead is closed-but-present — keyed by the work bead's assignee
// identity. Returns "" when no owning session bead is found or it carries no
// template/agent_name to recover from. ZFC-safe: the route is the session
// bead's configured template, never a hardcoded role name.
func orphanedPoolAssignmentFallbackRoute(store beads.Store, assignee string) string {
	sb, ok := owningSessionBeadForAssignee(store, assignee)
	if !ok {
		return ""
	}
	return retiredSessionFallbackRoute(sb)
}

// owningSessionBeadForAssignee finds the session bead (open OR closed) whose
// identity matches assignee. It mirrors liveOpenSessionAssignmentExists's dual
// lookup — a direct Get on the bead-ID candidates derived from the assignee,
// then a label scan — but includes closed beads, because the owning session of
// an orphaned work bead has usually already been reaped (closed). Returns the
// first session bead whose sessionBeadAssigneeIdentities contains assignee.
func owningSessionBeadForAssignee(store beads.Store, assignee string) (beads.Bead, bool) {
	assignee = strings.TrimSpace(assignee)
	if store == nil || assignee == "" {
		return beads.Bead{}, false
	}
	for _, id := range directSessionBeadIDCandidates(assignee) {
		sb, err := store.Get(id)
		if err != nil || !isSessionBead(sb) {
			continue
		}
		for _, candidate := range sessionBeadAssigneeIdentities(sb) {
			if assignee == candidate {
				return sb, true
			}
		}
	}
	sessions, err := store.List(beads.ListQuery{
		Label:         sessionBeadLabel,
		IncludeClosed: true,
	})
	if err != nil {
		log.Printf("releaseOrphanedPoolAssignments: owning-session lookup failed for assignee %q: %v", assignee, err)
		return beads.Bead{}, false
	}
	for _, sb := range sessions {
		if !isSessionBead(sb) {
			continue
		}
		for _, candidate := range sessionBeadAssigneeIdentities(sb) {
			if assignee == candidate {
				return sb, true
			}
		}
	}
	return beads.Bead{}, false
}

// verifyReleasedPoolAssignment reads a pool work bead back immediately after
// releaseOrphanedPoolAssignment's release write and logs loudly when a
// concurrent claim raced in and set a foreign, non-empty assignee after the
// release. expectedAssignee is the assignee the release write installed (the
// empty string for the unconditional release path); a read-back that is
// non-empty and differs from it means a re-claim landed after our write.
//
// This makes the claim-AFTER-release ordering observable. The
// claim-BETWEEN-recheck-and-write ordering — a re-claim landing after
// liveWorkAssignmentStillReleasable but before the release write, which our
// write then clobbers — reads back empty and stays undetectable here without a
// store-level conditional-release (CAS) verb the fork does not yet have. This
// is observability only: it never changes the release decision.
func verifyReleasedPoolAssignment(store beads.Store, id, expectedAssignee string) {
	if store == nil || id == "" {
		return
	}
	sb, err := store.Get(id)
	if err != nil {
		log.Printf("releaseOrphanedPoolAssignments: verify-after read failed for %q: %v", id, err)
		return
	}
	observed := strings.TrimSpace(sb.Assignee)
	expected := strings.TrimSpace(expectedAssignee)
	if observed != "" && observed != expected {
		log.Printf("releaseOrphanedPoolAssignments: RELEASE RACE on %s: observed assignee %q after release write (expected %q); a concurrent claim raced the orphan release", id, observed, expected)
	}
}

// poolAssignmentReopenedReason is the diagnostic tag carried by every
// pool.assignment_reopened event emitted from the orphan-release path.
const poolAssignmentReopenedReason = "orphaned_pool_assignment"

// poolAssignmentReopenedEvent builds the typed observability event for a single
// reopened pool assignment. Kept pure (no recorder, no I/O) so the event shape
// is unit-testable independently of the reconcile tick that emits it.
func poolAssignmentReopenedEvent(r releasedPoolAssignment) events.Event {
	return events.Event{
		Type:    events.PoolAssignmentReopened,
		Actor:   "gc",
		Subject: r.ID,
		Message: "reopened orphaned pool work",
		Payload: api.PoolAssignmentReopenedPayloadJSON(r.ID, r.PrevAssignee, r.Route, poolAssignmentReopenedReason),
	}
}

// recordReleasedPoolAssignments logs each reopened pool assignment to stderr
// (preserving the operator-facing line) and emits a typed
// pool.assignment_reopened event per bead, so the reopen churn that precedes a
// duplicate-dispatch race is observable via `gc events` / the SSE stream rather
// than only reconstructable from stderr after the fact. A nil recorder skips
// emission but still logs, so callers without an event bus stay functional.
func recordReleasedPoolAssignments(rec events.Recorder, stderr io.Writer, released []releasedPoolAssignment) {
	for _, r := range released {
		fmt.Fprintf(stderr, "released orphaned pool work: %s\n", r.ID) //nolint:errcheck // best-effort stderr
		if rec != nil {
			rec.Record(poolAssignmentReopenedEvent(r))
		}
	}
}

func liveOpenSessionAssignmentExists(store beads.Store, assignee string) bool {
	assignee = strings.TrimSpace(assignee)
	if store == nil || assignee == "" {
		return false
	}
	if liveSessionBeadExistsByIdentity(store, assignee) {
		return true
	}
	// NOTE: this call site intentionally keeps a label-only query — not
	// the Type+Label union from session.ListAllSessionBeads. The
	// orphan-release tests (TestReleaseOrphanedPoolAssignments_*) set up
	// city session beads with Type=session but no gc:session label and
	// assert that rig work pointing at a session_name only reachable via
	// the typed bead IS released. Switching this query to the union
	// would surface those typed beads as "live" and cause the work to
	// be skipped instead of released, regressing
	// ReopensRigStoreMissingPoolAssignee and
	// ReleasesRigWorkAssignedToUnreachableOpenSession. The label-loss
	// bug this PR is fixing manifests in the snapshot/list/reconciler
	// paths; orphan release continues to treat the label as the
	// authoritative liveness signal.
	sessions, err := store.List(beads.ListQuery{
		Label: sessionBeadLabel,
		Live:  true,
	})
	if err != nil {
		log.Printf("releaseOrphanedPoolAssignments: live session validation failed for assignee %q: %v", assignee, err)
		return true
	}
	for _, sb := range sessions {
		if sb.Status == "closed" || !isSessionBead(sb) {
			continue
		}
		for _, id := range sessionBeadAssigneeIdentities(sb) {
			if assignee == id {
				return true
			}
		}
	}
	return false
}

func liveSessionBeadExistsByIdentity(store beads.Store, assignee string) bool {
	for _, id := range directSessionBeadIDCandidates(assignee) {
		sb, err := store.Get(id)
		if err != nil {
			continue
		}
		if sb.Status == "closed" || !isSessionBead(sb) {
			continue
		}
		for _, candidate := range sessionBeadAssigneeIdentities(sb) {
			if assignee == candidate {
				return true
			}
		}
	}
	return false
}

func directSessionBeadIDCandidates(assignee string) []string {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return nil
	}
	candidates := []string{assignee}
	if idx := strings.LastIndex(assignee, "-mc-"); idx >= 0 {
		candidates = append(candidates, assignee[idx+1:])
	}
	return candidates
}

func liveWorkAssignmentStillReleasable(store beads.Store, id, assignee string) bool {
	id = strings.TrimSpace(id)
	if store == nil || id == "" {
		return false
	}
	work, err := store.List(beads.ListQuery{
		Status: "in_progress",
		Live:   true,
	})
	if err != nil {
		log.Printf("releaseOrphanedPoolAssignments: live work validation failed for %q: %v", id, err)
		return false
	}
	for _, wb := range work {
		if wb.ID != id {
			continue
		}
		return strings.TrimSpace(wb.Assignee) == strings.TrimSpace(assignee)
	}
	return false
}

func assigneePreservesNamedSessionRoute(cfg *config.City, cityPath, template, assignee, workStoreRef string, storeRefAware bool) bool {
	if cfg == nil {
		return false
	}
	spec, ok := findNamedSessionSpec(cfg, cfg.EffectiveCityName(), assignee)
	if !ok {
		return false
	}
	if namedSessionBackingTemplate(spec) != template {
		return false
	}
	if !storeRefAware {
		return true
	}
	return assignedWorkStoreRefForAgent(cityPath, cfg, spec.Agent) == workStoreRef
}

func stringPtr(s string) *string { return &s }
