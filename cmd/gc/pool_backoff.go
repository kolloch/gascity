package main

import (
	"sync"
	"time"
)

// defaultPoolBackoffCooldown is how long the underfull condition (scale_check
// demand exceeding truly-claimable routed work) must persist before new pool
// spawns are capped. Ten minutes leaves room for transient race conditions
// between scale_check and polecat claim to recover before suppressing.
const defaultPoolBackoffCooldown = 10 * time.Minute

// poolBackoffNow returns the wall-clock time used by the back-off cooldown
// comparison. It exists because buildDesiredStateWithSessionBeads is called
// with a stable beaconTime captured at controller startup, which would freeze
// the cooldown if used directly. Tests override this to control time.
var poolBackoffNow = time.Now

// PoolBackoffState tracks per-template under-claimability across reconciler
// ticks. When scale_check demand exceeds the independently-computed claimable
// count and the condition persists past the cooldown, scale_check demand is
// capped at the claimable value to avoid spawn-drain-spawn churn.
//
// The bead ga-bps motivates this: a zack polecat pool spawned ~72 polecats
// over 5 hours while only 2 routed beads existed (neither claimable). Each
// empty-spawn produced an empty-hook escalation. Without sanity-checking the
// scale_check signal, a buggy or stale demand counter keeps pulling on a
// queue that has nothing to give.
//
// The min-active-sessions floor is honored downstream by the existing
// min-fill pass in applyNestedCaps; this layer only caps the new-demand
// portion of scale_check, never the floor.
type PoolBackoffState struct {
	mu               sync.Mutex
	firstUnderfullAt map[string]time.Time
	cooldown         time.Duration
}

// PoolBackoffDecision records a per-template back-off outcome for one tick.
// Telemetry and trace consumers project these into events or log lines so
// operators can see why pools are spawning fewer (or zero) sessions than
// the raw scale_check value would suggest.
type PoolBackoffDecision struct {
	Template       string
	ScaleCheck     int           // input demand from scale_check
	Claimable      int           // independent count of truly-claimable routed beads
	AdjustedDemand int           // demand after back-off (== Claimable when Suppressed)
	Suppressed     bool          // true iff back-off capped demand this tick
	UnderfullSince time.Duration // how long the underfull condition has persisted
}

// NewPoolBackoffState constructs a tracker with the given cooldown. A
// non-positive value falls back to defaultPoolBackoffCooldown.
func NewPoolBackoffState(cooldown time.Duration) *PoolBackoffState {
	if cooldown <= 0 {
		cooldown = defaultPoolBackoffCooldown
	}
	return &PoolBackoffState{
		firstUnderfullAt: make(map[string]time.Time),
		cooldown:         cooldown,
	}
}

// Apply transforms scaleCheckCounts so that any template whose underfull
// condition (scale_check > claimable) has persisted past the cooldown is
// capped at the claimable value. The original map is not mutated; a fresh
// map is returned. Templates with no entry in claimableCounts pass through
// unchanged — back-off requires both sides of the comparison to be
// meaningful.
//
// Returns the adjusted counts and, for each underfull template, a decision
// describing the outcome (suppressed or merely observed). Decisions are
// returned in non-deterministic order; callers that emit telemetry should
// sort or annotate as needed.
//
// A nil receiver is safe and returns the input unchanged with no decisions.
func (s *PoolBackoffState) Apply(scaleCheckCounts, claimableCounts map[string]int, now time.Time) (map[string]int, []PoolBackoffDecision) {
	if s == nil {
		return scaleCheckCounts, nil
	}
	if len(scaleCheckCounts) == 0 {
		// Drop any stale per-template state so a vanished template doesn't
		// keep its underfull timer indefinitely.
		s.mu.Lock()
		s.firstUnderfullAt = make(map[string]time.Time)
		s.mu.Unlock()
		return scaleCheckCounts, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	adjusted := make(map[string]int, len(scaleCheckCounts))
	var decisions []PoolBackoffDecision
	for template, scaleCount := range scaleCheckCounts {
		claimable, hasClaimable := claimableCounts[template]
		if !hasClaimable {
			adjusted[template] = scaleCount
			delete(s.firstUnderfullAt, template)
			continue
		}
		if scaleCount <= claimable {
			adjusted[template] = scaleCount
			delete(s.firstUnderfullAt, template)
			continue
		}
		first, marked := s.firstUnderfullAt[template]
		if !marked {
			s.firstUnderfullAt[template] = now
			adjusted[template] = scaleCount
			decisions = append(decisions, PoolBackoffDecision{
				Template:       template,
				ScaleCheck:     scaleCount,
				Claimable:      claimable,
				AdjustedDemand: scaleCount,
				Suppressed:     false,
				UnderfullSince: 0,
			})
			continue
		}
		sinceUnderfull := now.Sub(first)
		if sinceUnderfull < s.cooldown {
			adjusted[template] = scaleCount
			decisions = append(decisions, PoolBackoffDecision{
				Template:       template,
				ScaleCheck:     scaleCount,
				Claimable:      claimable,
				AdjustedDemand: scaleCount,
				Suppressed:     false,
				UnderfullSince: sinceUnderfull,
			})
			continue
		}
		adjusted[template] = claimable
		decisions = append(decisions, PoolBackoffDecision{
			Template:       template,
			ScaleCheck:     scaleCount,
			Claimable:      claimable,
			AdjustedDemand: claimable,
			Suppressed:     true,
			UnderfullSince: sinceUnderfull,
		})
	}
	// Drop tracker entries for templates that no longer appear in
	// scaleCheckCounts (template removed from config, pool suspended, etc.).
	for template := range s.firstUnderfullAt {
		if _, ok := scaleCheckCounts[template]; !ok {
			delete(s.firstUnderfullAt, template)
		}
	}
	return adjusted, decisions
}

// Reset clears tracked under-claimability state. Useful for tests and for
// forcing a fresh evaluation after a known config change.
func (s *PoolBackoffState) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.firstUnderfullAt = make(map[string]time.Time)
}
