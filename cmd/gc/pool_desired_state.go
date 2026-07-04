package main

import (
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// SessionRequest represents a single session the reconciler should start.
type SessionRequest struct {
	Template      string // agent template qualified name (e.g., "gascity/claude")
	BeadPriority  int    // priority of the driving work bead
	Tier          string // "resume" (in-progress work with assigned session) or "new" (ready unassigned work)
	SessionBeadID string // concrete session to preserve for resume or in-flight new demand
	WorkBeadID    string // the work bead driving this request
}

func beadPriority(b beads.Bead) int {
	if b.Priority != nil {
		return *b.Priority
	}
	return 0
}

// PoolDesiredState holds the desired state for a single agent template.
type PoolDesiredState struct {
	Template string
	Requests []SessionRequest // accepted requests (within all caps)
}

// ReconcileDecision is the output of the nested cap enforcement.
type ReconcileDecision struct {
	Start []SessionRequest // sessions to start
	// Stop is computed by the reconciler by comparing Start against running sessions.
}

func PoolDesiredCounts(states []PoolDesiredState) map[string]int {
	if len(states) == 0 {
		return nil
	}
	counts := make(map[string]int, len(states))
	for _, state := range states {
		counts[state.Template] = len(state.Requests)
	}
	return counts
}

// ComputePoolDesiredStates computes the desired state for all pool agents.
// assignedWorkBeads contains actionable assigned work beads only: in-progress
// work and open work that was already proven ready upstream. Routed but
// unassigned pool queue work must not be passed here; new-session demand comes
// from scale_check, while this function only preserves sessions that already
// own actionable work.
// Each bead's gc.routed_to determines which agent template it belongs to.
// scaleCheckCounts maps agent template → new session demand from scale_check.
// Pass nil for either when unavailable.
func ComputePoolDesiredStates(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionBeads []beads.Bead,
	scaleCheckCounts map[string]int,
) []PoolDesiredState {
	return computePoolDesiredStates(cfg, assignedWorkBeads, sessionBeads, scaleCheckCounts, nil)
}

func ComputePoolDesiredStatesTraced(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionBeads []beads.Bead,
	scaleCheckCounts map[string]int,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	return computePoolDesiredStates(cfg, assignedWorkBeads, sessionBeads, scaleCheckCounts, trace)
}

func computePoolDesiredStates(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionBeads []beads.Bead,
	scaleCheckCounts map[string]int,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	// Build reverse lookup: any identifier → session bead ID.
	// Assignee on work beads may be a bead ID, session name, alias, or
	// a prior alias preserved in alias_history. Resume-tier dispatch
	// drops in-progress work whose owning session can't be resolved
	// from this map, so missing identities cause live sessions to look
	// orphaned and let a duplicate spawn for the same bead.
	assigneeToSessionBeadID := make(map[string]string)
	sessionBeadTemplate := make(map[string]string)
	namedSessionBeadIDs := make(map[string]bool)
	for _, sb := range sessionBeads {
		if sb.Status == "closed" {
			continue
		}
		template := strings.TrimSpace(normalizedSessionTemplate(sb, cfg))
		if template != "" {
			sessionBeadTemplate[sb.ID] = template
		}
		for _, id := range sessionBeadAssigneeIdentities(sb) {
			assigneeToSessionBeadID[id] = sb.ID
		}
		if isNamedSessionBead(sb) {
			namedSessionBeadIDs[sb.ID] = true
		}
	}

	var resumeRequests []SessionRequest

	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended {
			continue
		}
		if !agent.SupportsGenericEphemeralSessions() {
			continue
		}
		template := agent.QualifiedName()

		// Resume tier: actionable assigned work beads whose assignee resolves
		// to a non-closed session bead. These sessions must stay alive.
		for _, wb := range assignedWorkBeads {
			routedTo := wb.Metadata["gc.routed_to"]
			if wb.Status != "in_progress" && wb.Status != "open" {
				continue
			}
			assignee := strings.TrimSpace(wb.Assignee)
			if assignee == "" {
				continue
			}
			sessionBeadID := assigneeToSessionBeadID[assignee]
			if routedTo == "" && sessionBeadID != "" {
				routedTo = sessionBeadTemplate[sessionBeadID]
				if routedTo == "" && len(cfg.Agents) == 1 {
					routedTo = cfg.Agents[0].QualifiedName()
				}
			}
			if sessionBeadID != "" {
				sessionTemplate := strings.TrimSpace(sessionBeadTemplate[sessionBeadID])
				if sessionTemplate != "" && routedTo != "" && routedTo != sessionTemplate {
					continue
				}
			}
			if routedTo != template {
				continue
			}
			if sessionBeadID != "" {
				// Named-session beads are materialized by the named-session
				// loop in buildDesiredState, not by the pool path. Skipping
				// here prevents realizePoolDesiredSessions from renaming the
				// canonical named identity to a phantom "{name}-1" pool
				// instance — which would create two desired sessions for the
				// same agent even when max_active_sessions=1.
				if namedSessionBeadIDs[sessionBeadID] {
					continue
				}
				resumeRequests = append(resumeRequests, SessionRequest{
					Template:      template,
					BeadPriority:  beadPriority(wb),
					Tier:          "resume",
					SessionBeadID: sessionBeadID,
					WorkBeadID:    wb.ID,
				})
			}
			// Else: assignee set but session closed/unknown — orphaned
			// work, not our job to respawn.
		}
	}

	limits := newNestedCapLimits(cfg)
	usage := acceptedNestedCapUsage(limits, resumeRequests)
	allRequests := append([]SessionRequest(nil), resumeRequests...)
	resumeSessionBeadIDs := make(map[string]struct{}, len(resumeRequests))
	for _, req := range resumeRequests {
		if req.SessionBeadID != "" {
			resumeSessionBeadIDs[req.SessionBeadID] = struct{}{}
		}
	}
	inFlightNewRequests := poolInFlightNewRequests(cfg, sessionBeads, resumeSessionBeadIDs)
	awakeIdleRequests := awakeIdleLivePoolRequests(cfg, sessionBeads, resumeSessionBeadIDs)
	wakableAsleepRequests := wakableAsleepPoolRequests(cfg, sessionBeads, resumeSessionBeadIDs)

	// Merge scale_check demand. In bead-backed reconciliation, scale_check is
	// the authoritative signal for new unassigned demand only; resume requests
	// are calculated independently from assigned work and must not be deducted
	// from that count. Pool-created sessions that have not claimed work yet
	// represent already-spent new demand, so they occupy the first new-demand
	// slots explicitly before anonymous creates are materialized. The fill
	// order attributes one reuse slot per existing pre-claim/idle worker before
	// any fresh spawn:
	//
	//  1. in-flight (creating/pending_create) — a worker mid-creation.
	//  2. awake-idle (state active/awake, unclaimed) — the active-state
	//     continuation of the same pre-claim worker; it will claim on its next
	//     work_query poll, so a second fresh spawn for that bead is redundant
	//     (ga-50hw). Chosen before asleep because it is already awake — waking
	//     an asleep sibling for a bead an awake worker will claim is the exact
	//     redundant spawn this tier removes.
	//  3. wakable asleep — reuses an existing worktree instead of a fresh one
	//     (ga-htl).
	//
	// Drained workers (done-sequence, about to exit) are excluded from tiers 2
	// and 3, so an about-to-drain worker never masks real demand.
	if len(scaleCheckCounts) > 0 {
		for i := range cfg.Agents {
			agent := &cfg.Agents[i]
			if agent.Suspended {
				continue
			}
			template := agent.QualifiedName()
			scaleCount, ok := scaleCheckCounts[template]
			if !ok {
				continue
			}
			newCount := capNewDemandCount(limits, usage, agent, scaleCount)
			inFlight := inFlightNewRequests[template]
			inFlightCount := minInt(len(inFlight), newCount)
			awakeIdle := awakeIdleRequests[template]
			awakeIdleCount := minInt(len(awakeIdle), newCount-inFlightCount)
			asleep := wakableAsleepRequests[template]
			asleepCount := minInt(len(asleep), newCount-inFlightCount-awakeIdleCount)
			if scaleCount > 0 && trace != nil && (len(inFlight) > 0 || len(awakeIdle) > 0 || len(asleep) > 0) {
				trace.recordDecision(string(TraceSitePoolInFlightReuse), template, "", string(TraceReasonInFlightReuse), "accepted", traceRecordPayload{
					"scale_check":   scaleCount,
					"in_flight":     len(inFlight),
					"reused":        inFlightCount,
					"awake_idle":    len(awakeIdle),
					"awake_reused":  awakeIdleCount,
					"asleep":        len(asleep),
					"woken":         asleepCount,
					"anonymous_new": newCount - inFlightCount - awakeIdleCount - asleepCount,
				}, nil, "")
			}
			for j := 0; j < inFlightCount; j++ {
				req := inFlight[j]
				allRequests = append(allRequests, req)
				usage.accept(req, limits)
			}
			for j := 0; j < awakeIdleCount; j++ {
				req := awakeIdle[j]
				allRequests = append(allRequests, req)
				usage.accept(req, limits)
			}
			for j := 0; j < asleepCount; j++ {
				req := asleep[j]
				allRequests = append(allRequests, req)
				usage.accept(req, limits)
			}
			for j := inFlightCount + awakeIdleCount + asleepCount; j < newCount; j++ {
				req := SessionRequest{
					Template: template,
					Tier:     "new",
				}
				allRequests = append(allRequests, req)
				usage.accept(req, limits)
			}
		}
	}

	return applyNestedCaps(cfg, allRequests, trace)
}

func poolInFlightNewRequests(cfg *config.City, sessionBeads []beads.Bead, resumeSessionBeadIDs map[string]struct{}) map[string][]SessionRequest {
	requests := make(map[string][]SessionRequest)
	sortedSessionBeads := append([]beads.Bead(nil), sessionBeads...)
	sort.SliceStable(sortedSessionBeads, func(i, j int) bool {
		if !sortedSessionBeads[i].CreatedAt.Equal(sortedSessionBeads[j].CreatedAt) {
			return sortedSessionBeads[i].CreatedAt.Before(sortedSessionBeads[j].CreatedAt)
		}
		return sortedSessionBeads[i].ID < sortedSessionBeads[j].ID
	})
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended || !agent.SupportsGenericEphemeralSessions() {
			continue
		}
		template := agent.QualifiedName()
		for _, sb := range sortedSessionBeads {
			if sb.ID == "" || sb.Status == "closed" {
				continue
			}
			if _, ok := resumeSessionBeadIDs[sb.ID]; ok {
				continue
			}
			if !isEphemeralSessionBeadForAgent(sb, agent) || !isPoolManagedSessionBead(sb) {
				continue
			}
			if normalizedSessionTemplate(sb, cfg) != template {
				continue
			}
			if !poolSessionConsumesNewDemand(sb) {
				continue
			}
			requests[template] = append(requests[template], SessionRequest{
				Template:      template,
				Tier:          "new",
				SessionBeadID: sb.ID,
			})
		}
	}
	return requests
}

func poolSessionConsumesNewDemand(session beads.Bead) bool {
	if strings.TrimSpace(session.Metadata["pending_create_claim"]) == boolMetadata(true) {
		return true
	}
	// This pure desired-state pass has no reconciler clock. Creating sessions
	// still represent already-spent new demand; lifecycle code owns stale
	// creating recovery with its clock-aware predicate.
	return strings.TrimSpace(session.Metadata["state"]) == "creating"
}

// awakeIdleLivePoolRequests returns per-template SessionRequests for awake,
// idle, live ephemeral pool beads — sessions that have started (state active
// or awake) but have not yet claimed work. Such a session will claim a routed
// pool bead on its next work_query poll, so counting it as already-spent new
// demand prevents the scaler from spawning a redundant fresh session for a bead
// an existing worker is about to claim (e.g. a reopened pool bead whose spawned
// worker has advanced creating->active but has not yet claimed). This is the
// active-state continuation of poolInFlightNewRequests, which covers the same
// worker while it is still creating/pending_create; ordering the two tiers
// adjacently attributes both pre-claim lifecycle stages of one worker to the
// demand it already satisfies (ga-50hw).
//
// The state active/awake requirement is the under-provisioning guard: a worker
// that has run `gc runtime drain-ack` lands in state=drained (or asleep+
// sleep_reason=drained) and is about to EXIT, not about to claim — neither is
// active/awake, so a drained worker never masks demand and the reopened bead
// still spawns fresh. The one residual window is a worker that has reassigned
// its bead to the refinery but not yet drain-acked: it is briefly active with
// no assigned work and would absorb a demand slot. That is self-healing, not a
// strand — it drains within the same done-sequence, and the next reconcile tick
// re-evaluates without it and spawns fresh, matching the transient semantics of
// the asleep tier. poolSessionConsumesNewDemand beads (pending_create_claim race) are
// excluded to avoid double-counting the same worker in the in-flight tier;
// resume-set, named, manual, and non-pool-managed beads are excluded for the
// same reasons as the in-flight and asleep tiers. Beads are sorted oldest-first
// by created_at to match those tiers for deterministic selection.
func awakeIdleLivePoolRequests(cfg *config.City, sessionBeads []beads.Bead, resumeSessionBeadIDs map[string]struct{}) map[string][]SessionRequest {
	requests := make(map[string][]SessionRequest)
	sortedSessionBeads := append([]beads.Bead(nil), sessionBeads...)
	sort.SliceStable(sortedSessionBeads, func(i, j int) bool {
		if !sortedSessionBeads[i].CreatedAt.Equal(sortedSessionBeads[j].CreatedAt) {
			return sortedSessionBeads[i].CreatedAt.Before(sortedSessionBeads[j].CreatedAt)
		}
		return sortedSessionBeads[i].ID < sortedSessionBeads[j].ID
	})
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended || !agent.SupportsGenericEphemeralSessions() {
			continue
		}
		// Canonical-singleton agents (max_active_sessions=1) never have the
		// multi-worker redundant-spawn problem this offset addresses, and their
		// reuse/dedup — including preferring the canonical identity over a stale
		// "-N" duplicate — is owned by realizePoolDesiredSessions'
		// findReusableCanonicalNonExpandingPoolSessionBead path. Emitting a
		// specific SessionBeadID here could pin a stale duplicate ahead of the
		// canonical, so leave singleton reuse to that path.
		if agent.UsesCanonicalSingletonPoolIdentity() {
			continue
		}
		template := agent.QualifiedName()
		for _, sb := range sortedSessionBeads {
			if sb.ID == "" || sb.Status == "closed" {
				continue
			}
			if _, ok := resumeSessionBeadIDs[sb.ID]; ok {
				continue
			}
			if !isEphemeralSessionBeadForAgent(sb, agent) || !isPoolManagedSessionBead(sb) {
				continue
			}
			if isNamedSessionBead(sb) || isManualSessionBeadForAgent(sb, agent) {
				continue
			}
			if normalizedSessionTemplate(sb, cfg) != template {
				continue
			}
			// Only sessions that are awake and free to claim. Creating/pending
			// beads belong to the in-flight tier; asleep beads to the wake
			// tier; drained beads are about to exit. active/awake selects
			// exactly the started-but-unclaimed workers and excludes all three.
			if state := strings.TrimSpace(sb.Metadata["state"]); state != "active" && state != "awake" {
				continue
			}
			if poolSessionConsumesNewDemand(sb) {
				// Pending-create race (pending_create_claim still set) —
				// already counted by the in-flight tier.
				continue
			}
			requests[template] = append(requests[template], SessionRequest{
				Template:      template,
				Tier:          "new",
				SessionBeadID: sb.ID,
			})
		}
	}
	return requests
}

// wakableAsleepPoolRequests returns per-template SessionRequests for asleep
// ephemeral pool beads that can be woken to satisfy new scale_check demand.
// Drained beads, named-session beads, manual sessions, beads already in the
// resume set, and beads still pending their initial start (counted by
// poolInFlightNewRequests via poolSessionConsumesNewDemand) are excluded —
// those have their own lifecycle paths. Beads are sorted oldest-first by
// created_at, matching poolInFlightNewRequests for deterministic selection
// (ga-htl).
func wakableAsleepPoolRequests(cfg *config.City, sessionBeads []beads.Bead, resumeSessionBeadIDs map[string]struct{}) map[string][]SessionRequest {
	requests := make(map[string][]SessionRequest)
	sortedSessionBeads := append([]beads.Bead(nil), sessionBeads...)
	sort.SliceStable(sortedSessionBeads, func(i, j int) bool {
		if !sortedSessionBeads[i].CreatedAt.Equal(sortedSessionBeads[j].CreatedAt) {
			return sortedSessionBeads[i].CreatedAt.Before(sortedSessionBeads[j].CreatedAt)
		}
		return sortedSessionBeads[i].ID < sortedSessionBeads[j].ID
	})
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended || !agent.SupportsGenericEphemeralSessions() {
			continue
		}
		template := agent.QualifiedName()
		for _, sb := range sortedSessionBeads {
			if sb.ID == "" || sb.Status == "closed" {
				continue
			}
			if _, ok := resumeSessionBeadIDs[sb.ID]; ok {
				continue
			}
			if !isEphemeralSessionBeadForAgent(sb, agent) || !isPoolManagedSessionBead(sb) {
				continue
			}
			if isNamedSessionBead(sb) || isManualSessionBeadForAgent(sb, agent) {
				continue
			}
			if normalizedSessionTemplate(sb, cfg) != template {
				continue
			}
			if strings.TrimSpace(sb.Metadata["state"]) != "asleep" {
				continue
			}
			if isDrainedSessionBead(sb) {
				continue
			}
			if poolSessionConsumesNewDemand(sb) {
				// Pending-create asleep bead — already counted as in-flight.
				continue
			}
			requests[template] = append(requests[template], SessionRequest{
				Template:      template,
				Tier:          "new",
				SessionBeadID: sb.ID,
			})
		}
	}
	return requests
}

// applyNestedCaps enforces workspace, rig, and agent max_active_sessions caps.
// Accepts requests in priority order, rejecting any that would exceed a cap.
func applyNestedCaps(cfg *config.City, requests []SessionRequest, trace *sessionReconcilerTraceCycle) []PoolDesiredState {
	// Sort by priority DESC, resume tier first within same priority.
	sort.SliceStable(requests, func(i, j int) bool {
		if requests[i].BeadPriority != requests[j].BeadPriority {
			return requests[i].BeadPriority > requests[j].BeadPriority
		}
		// Resume tier before new tier at same priority.
		if requests[i].Tier != requests[j].Tier {
			return requests[i].Tier == "resume"
		}
		return false
	})

	limits := newNestedCapLimits(cfg)
	usage := newNestedCapUsage()

	// Walk sorted requests, accepting each if all caps have room.
	accepted := make(map[string][]SessionRequest) // template → accepted requests

	for _, req := range requests {
		template := req.Template
		if usage.isDuplicateSessionRequest(req) {
			continue
		}
		if site, reason, payload, rejected := usage.rejection(req, limits); rejected {
			if trace != nil {
				trace.recordDecision(site, template, "", reason, "rejected", payload, nil, "")
			}
			continue
		}

		// Accept.
		accepted[template] = append(accepted[template], req)
		if trace != nil {
			trace.recordDecision("reconciler.pool.accept", template, "", "cap", "accepted", traceRecordPayload{
				"tier": req.Tier,
			}, nil, "")
		}
		usage.accept(req, limits)
	}

	// Fill agent mins (if caps allow).
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended {
			continue
		}
		template := agent.QualifiedName()
		minSess := agent.EffectiveMinActiveSessions()
		for usage.agentCount[template] < minSess {
			req := SessionRequest{
				Template: template,
				Tier:     "new",
			}
			if _, _, _, rejected := usage.rejection(req, limits); rejected {
				break
			}
			accepted[template] = append(accepted[template], req)
			if trace != nil {
				trace.recordDecision("reconciler.pool.min_fill", template, "", "min_fill", "accepted", traceRecordPayload{
					"min":     minSess,
					"current": usage.agentCount[template],
					"tier":    "new",
				}, nil, "")
			}
			usage.accept(req, limits)
		}
	}

	// Build output.
	var result []PoolDesiredState
	for template, reqs := range accepted {
		result = append(result, PoolDesiredState{
			Template: template,
			Requests: reqs,
		})
	}
	// Stable output order.
	sort.Slice(result, func(i, j int) bool {
		return result[i].Template < result[j].Template
	})
	return result
}

type nestedCapLimits struct {
	workspaceMax int
	rigMax       map[string]int
	agentMax     map[string]int
	agentRig     map[string]string
}

type nestedCapUsage struct {
	agentCount      map[string]int
	rigCount        map[string]int
	workspaceCount  int
	seenSessionBead map[string]bool
}

func newNestedCapLimits(cfg *config.City) nestedCapLimits {
	limits := nestedCapLimits{
		workspaceMax: -1,
		rigMax:       make(map[string]int),
		agentMax:     make(map[string]int),
		agentRig:     make(map[string]string),
	}
	if cfg.Workspace.MaxActiveSessions != nil {
		limits.workspaceMax = *cfg.Workspace.MaxActiveSessions
	}
	for _, rig := range cfg.Rigs {
		if rig.MaxActiveSessions != nil {
			limits.rigMax[rig.Name] = *rig.MaxActiveSessions
		} else {
			limits.rigMax[rig.Name] = -1
		}
	}
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		template := agent.QualifiedName()
		limits.agentRig[template] = agent.Dir
		resolved := agent.ResolvedMaxActiveSessions(cfg)
		if resolved != nil {
			limits.agentMax[template] = *resolved
		} else {
			limits.agentMax[template] = -1
		}
	}
	return limits
}

func newNestedCapUsage() nestedCapUsage {
	return nestedCapUsage{
		agentCount:      make(map[string]int),
		rigCount:        make(map[string]int),
		seenSessionBead: make(map[string]bool),
	}
}

func acceptedNestedCapUsage(limits nestedCapLimits, requests []SessionRequest) nestedCapUsage {
	usage := newNestedCapUsage()
	sorted := append([]SessionRequest(nil), requests...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].BeadPriority != sorted[j].BeadPriority {
			return sorted[i].BeadPriority > sorted[j].BeadPriority
		}
		if sorted[i].Tier != sorted[j].Tier {
			return sorted[i].Tier == "resume"
		}
		return false
	})
	for _, req := range sorted {
		if usage.canAccept(req, limits) {
			usage.accept(req, limits)
		}
	}
	return usage
}

func capNewDemandCount(limits nestedCapLimits, usage nestedCapUsage, agent *config.Agent, demand int) int {
	if demand <= 0 {
		return 0
	}
	template := agent.QualifiedName()
	remaining := demand
	if agentMax := limits.agentMax[template]; agentMax >= 0 {
		remaining = minInt(remaining, agentMax-usage.agentCount[template])
	}
	if rig := limits.agentRig[template]; rig != "" {
		rigMax, ok := limits.rigMax[rig]
		if !ok {
			rigMax = -1
		}
		if rigMax >= 0 {
			remaining = minInt(remaining, rigMax-usage.rigCount[rig])
		}
	}
	if limits.workspaceMax >= 0 {
		remaining = minInt(remaining, limits.workspaceMax-usage.workspaceCount)
	}
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (u nestedCapUsage) canAccept(req SessionRequest, limits nestedCapLimits) bool {
	if u.isDuplicateSessionRequest(req) {
		return false
	}
	_, _, _, rejected := u.rejection(req, limits)
	return !rejected
}

func (u nestedCapUsage) isDuplicateSessionRequest(req SessionRequest) bool {
	return req.SessionBeadID != "" && u.seenSessionBead[req.SessionBeadID]
}

func (u nestedCapUsage) rejection(req SessionRequest, limits nestedCapLimits) (string, string, traceRecordPayload, bool) {
	template := req.Template
	if agentMax := limits.agentMax[template]; agentMax >= 0 && u.agentCount[template] >= agentMax {
		return "reconciler.pool.agent_cap", "agent_cap", traceRecordPayload{
			"agent_max": agentMax,
			"current":   u.agentCount[template],
			"tier":      req.Tier,
		}, true
	}
	rig := limits.agentRig[template]
	if rig != "" {
		rigMax, ok := limits.rigMax[rig]
		if !ok {
			rigMax = -1
		}
		if rigMax >= 0 && u.rigCount[rig] >= rigMax {
			return "reconciler.pool.rig_cap", "rig_cap", traceRecordPayload{
				"rig":     rig,
				"rig_max": rigMax,
				"current": u.rigCount[rig],
				"tier":    req.Tier,
			}, true
		}
	}
	if limits.workspaceMax >= 0 && u.workspaceCount >= limits.workspaceMax {
		return "reconciler.pool.workspace_cap", "workspace_cap", traceRecordPayload{
			"workspace_max": limits.workspaceMax,
			"current":       u.workspaceCount,
			"tier":          req.Tier,
		}, true
	}
	return "", "", nil, false
}

func (u *nestedCapUsage) accept(req SessionRequest, limits nestedCapLimits) {
	u.agentCount[req.Template]++
	if rig := limits.agentRig[req.Template]; rig != "" {
		u.rigCount[rig]++
	}
	u.workspaceCount++
	if req.SessionBeadID != "" {
		u.seenSessionBead[req.SessionBeadID] = true
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
