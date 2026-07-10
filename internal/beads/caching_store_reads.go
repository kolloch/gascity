package beads

import (
	"errors"
	"fmt"
	"time"
)

// List returns beads matching the query. Active-bead queries are served from
// cache when available. IncludeClosed queries merge cached active results with
// backing-store history when possible, preserving partial backing rows when bd
// reports corrupt entries and retaining cache-only fallback for transient
// non-partial bd failures.
func (c *CachingStore) List(query ListQuery) ([]Bead, error) {
	if !query.HasFilter() && !query.AllowScan {
		return nil, fmt.Errorf("listing beads: %w", ErrQueryRequiresScan)
	}
	// The cache only holds the issues tier (PrimeActive/Prime call the
	// backing store without a TierMode). Wisps and union queries must
	// reach the backing store directly so we do not return a stale or
	// incomplete snapshot of the wisps table.
	if query.TierMode != TierIssues {
		return c.backing.List(query)
	}
	if query.Live || query.ParentID != "" {
		c.mu.RLock()
		startSeq := c.mutationSeq
		c.mu.RUnlock()
		items, err := c.backing.List(query)
		if err == nil {
			items = c.refreshCachedBeads(query, startSeq, items)
		}
		return items, err
	}

	// Active-bead path: serve from cache after a bounded per-ID refresh of any
	// dirty rows. PrimeActive loads the full active set (open + in_progress),
	// so active-only queries are complete even before the history prime
	// finishes. On overlay decline (dirty set over cap, backing.Get failure, or
	// cache not servable) the read takes the old full-scan backing fallback.
	var cached []Bead
	if err := c.readCacheWithOverlay(c.cacheServableLocked, func(suppressed map[string]struct{}) {
		cached = make([]Bead, 0, len(c.beads))
		for _, b := range c.beads {
			if _, gone := suppressed[b.ID]; gone {
				continue
			}
			if !query.Matches(b) {
				continue
			}
			cached = append(cached, cloneBead(b))
		}
	}); err == nil {
		finish := func(items []Bead, err error) ([]Bead, error) {
			sortBeadsForQuery(items, query.Sort)
			if query.Limit > 0 && len(items) > query.Limit {
				items = items[:query.Limit]
			}
			return items, err
		}

		if !query.IncludesClosed() {
			return finish(cached, nil)
		}

		// The cache never has a complete closed-only or parent-history view, so
		// preserve the old backing-store behavior for those query shapes.
		if query.Status == "closed" || query.ParentID != "" {
			return c.backing.List(liveListQuery(query))
		}

		all, err := c.backing.List(liveListQuery(query))
		if err != nil {
			if !IsPartialResult(err) {
				return finish(cached, nil)
			}
		}

		seen := make(map[string]bool, len(cached))
		for _, b := range cached {
			seen[b.ID] = true
		}
		for _, b := range all {
			if seen[b.ID] {
				continue
			}
			cached = append(cached, b)
			seen[b.ID] = true
		}
		return finish(cached, err)
	}
	return c.backing.List(liveListQuery(query))
}

func liveListQuery(query ListQuery) ListQuery {
	query.Live = true
	return query
}

// CachedList returns query results from the in-memory cache only. The boolean
// reports whether the cache was initialized enough to answer without touching
// the backing store. Dirty entries are returned from the last observed
// snapshot; callers must treat this as a read model that may lag writes or
// reconciliation by one tick.
func (c *CachingStore) CachedList(query ListQuery) ([]Bead, bool) {
	if query.TierMode != TierIssues {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.state != cacheLive && c.state != cachePartial {
		return nil, false
	}
	if c.primePartialErr != nil {
		return nil, false
	}
	cached := make([]Bead, 0, len(c.beads))
	for _, b := range c.beads {
		if !query.Matches(b) {
			continue
		}
		cached = append(cached, cloneBead(b))
	}
	sortBeadsForQuery(cached, query.Sort)
	if query.Limit > 0 && len(cached) > query.Limit {
		cached = cached[:query.Limit]
	}
	return cached, true
}

func (c *CachingStore) refreshCachedBeads(query ListQuery, startSeq uint64, items []Bead) []Bead {
	refreshedParents := make(map[string]Bead)
	removedParents := make(map[string]struct{})
	for _, id := range c.staleParentCacheIDs(query.ParentID, items) {
		fresh, err := c.backing.Get(id)
		switch {
		case err == nil:
			refreshedParents[id] = cloneBead(fresh)
		case errors.Is(err, ErrNotFound):
			removedParents[id] = struct{}{}
		default:
			c.recordProblem("refresh parent cache during list", fmt.Errorf("%s: %w", id, err))
		}
	}
	if len(items) == 0 && len(refreshedParents) == 0 && len(removedParents) == 0 {
		return items
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != cacheLive && c.state != cachePartial {
		return items
	}
	now := time.Now()
	refreshed := make([]Bead, 0, len(items))
	for _, item := range items {
		if c.deletedSeq[item.ID] > startSeq {
			continue
		}
		if c.beadSeq[item.ID] > startSeq {
			current, ok := c.beads[item.ID]
			if ok && query.Matches(current) {
				refreshed = append(refreshed, cloneBead(current))
			}
			continue
		}
		if current, keep := c.recentLocalBeadConflictLocked(item.ID, item, now, false); keep {
			if query.Matches(current) {
				refreshed = append(refreshed, current)
			}
			continue
		}
		if c.beadSeq[item.ID] == startSeq {
			current, ok := c.beads[item.ID]
			if ok && current.Status == "closed" && item.Status != "closed" {
				continue
			}
		}
		c.beads[item.ID] = cloneBead(item)
		c.deps[item.ID] = depsFromBeadFields(item)
		delete(c.dirty, item.ID)
		delete(c.deletedSeq, item.ID)
		if !recentLocalMutation(c.localBeadAt[item.ID], now) {
			delete(c.beadSeq, item.ID)
			delete(c.localBeadAt, item.ID)
		}
		if query.Matches(item) {
			refreshed = append(refreshed, cloneBead(item))
		}
	}
	for id, bead := range refreshedParents {
		if c.deletedSeq[id] > startSeq || c.beadSeq[id] > startSeq {
			continue
		}
		if _, keep := c.recentLocalBeadConflictLocked(id, bead, now, false); keep {
			continue
		}
		c.beads[id] = bead
		c.deps[id] = depsFromBeadFields(bead)
		delete(c.dirty, id)
		delete(c.deletedSeq, id)
		if !recentLocalMutation(c.localBeadAt[id], now) {
			delete(c.beadSeq, id)
			delete(c.localBeadAt, id)
		}
	}
	for id := range removedParents {
		if c.deletedSeq[id] > startSeq || c.beadSeq[id] > startSeq {
			continue
		}
		if current, ok := c.beads[id]; ok && current.Status != "closed" && recentLocalMutation(c.localBeadAt[id], now) {
			continue
		}
		delete(c.beads, id)
		delete(c.deps, id)
		delete(c.dirty, id)
		delete(c.deletedSeq, id)
		delete(c.beadSeq, id)
		delete(c.localBeadAt, id)
	}
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	return refreshed
}

func (c *CachingStore) staleParentCacheIDs(parentID string, fresh []Bead) []string {
	if parentID == "" {
		return nil
	}

	freshIDs := make(map[string]struct{}, len(fresh))
	for _, item := range fresh {
		freshIDs[item.ID] = struct{}{}
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.state != cacheLive && c.state != cachePartial {
		return nil
	}

	var stale []string
	for id, bead := range c.beads {
		if bead.ParentID != parentID {
			continue
		}
		if _, ok := freshIDs[id]; ok {
			continue
		}
		stale = append(stale, id)
	}
	return stale
}

// ListOpen returns all cached beads, optionally filtered by status.
func (c *CachingStore) ListOpen(status ...string) ([]Bead, error) {
	query := ListQuery{AllowScan: true}
	if len(status) > 0 {
		query.Status = status[0]
	}
	return c.List(query)
}

// Get returns a single bead by ID from the cache or backing store.
func (c *CachingStore) Get(id string) (Bead, error) {
	c.mu.RLock()
	if _, deleted := c.deletedSeq[id]; deleted {
		c.mu.RUnlock()
		return Bead{}, ErrNotFound
	}
	if _, mutated := c.beadSeq[id]; mutated {
		if _, dirty := c.dirty[id]; !dirty {
			if b, ok := c.beads[id]; ok {
				c.mu.RUnlock()
				return cloneBead(b), nil
			}
		}
	}
	if c.state == cacheLive || c.state == cachePartial {
		if _, ok := c.dirty[id]; ok {
			startSeq := c.mutationSeq
			c.mu.RUnlock()
			fresh, err := c.backing.Get(id)
			if err != nil {
				return Bead{}, err
			}
			c.mu.Lock()
			if c.state != cacheLive && c.state != cachePartial {
				c.mu.Unlock()
				return fresh, nil
			}
			switch {
			case c.deletedSeq[id] > startSeq:
				c.mu.Unlock()
				return Bead{}, ErrNotFound
			case c.beadSeq[id] > startSeq:
				if _, stillDirty := c.dirty[id]; stillDirty {
					c.mu.Unlock()
					return c.backing.Get(id)
				}
				if current, ok := c.beads[id]; ok {
					c.mu.Unlock()
					return cloneBead(current), nil
				}
				c.mu.Unlock()
				return Bead{}, ErrNotFound
			}
			c.beads[id] = cloneBead(fresh)
			c.deps[id] = depsFromBeadFields(fresh)
			delete(c.dirty, id)
			delete(c.deletedSeq, id)
			delete(c.beadSeq, id)
			c.markFreshLocked(time.Now())
			c.updateStatsLocked()
			c.mu.Unlock()
			return fresh, nil
		}
		if b, ok := c.beads[id]; ok {
			c.mu.RUnlock()
			return cloneBead(b), nil
		}
		c.mu.RUnlock()
		return c.backing.Get(id)
	}
	c.mu.RUnlock()
	return c.backing.Get(id)
}

// dirtyOverlayMaxGets bounds the inline per-ID refresh a cached read performs
// before it declines the overlay and falls back to the full backing scan.
// Above the cap the read degrades to prior behavior — never worse.
const dirtyOverlayMaxGets = 8

// errDirtyOverlayFallback signals that a cached read must take its existing
// fallback path (backing.List / backing.Ready, each unchanged per site). It
// never escapes the read site.
var errDirtyOverlayFallback = errors.New("beads cache: dirty overlay fallback")

// cacheServableLocked reports whether the active read model can answer from
// cache: the cache is live or partial and the prime was not a partial error.
// Dirty is no longer a serve-blocker for List — dirty rows are refreshed in
// place by readCacheWithOverlay. Caller must hold c.mu (read or write).
func (c *CachingStore) cacheServableLocked() bool {
	return (c.state == cacheLive || c.state == cachePartial) && c.primePartialErr == nil
}

// readCacheWithOverlay serves a cached read after refreshing only the dirty
// rows, replacing the old "one dirty bead declines the whole cache" tripwire
// with a bounded per-bead overlay that generalizes the single-ID refresh in
// Get.
//
// gate reports, under the lock, whether the cache is servable for this read
// shape (cacheServableLocked for List; Ready additionally requires
// depsComplete). collect materializes the read from the cache and is invoked
// exactly once, while the lock is held, only after every dirty row has been
// refreshed from the backing store or confirmed absent — so no dirty row is
// ever served and no new dirty mark can slip in between the last refresh and
// the serve. suppressed holds IDs that backing.Get reported ErrNotFound this
// read; collect must omit them, matching what the old full backing.List would
// have returned for deleted rows. A suppressed id that a concurrent apply
// resurrects between fetch and re-lock is caught by retrySuppressedChurnLocked
// and re-fetched, so the serve never omits a now-live row.
//
// A non-nil error means the caller must take its existing fallback path: the
// dirty set exceeds dirtyOverlayMaxGets, a backing.Get failed with a
// non-NotFound error, the cache is not servable, or residual dirty churn
// survived the bounded retry. No backing I/O happens under c.mu.
func (c *CachingStore) readCacheWithOverlay(gate func() bool, collect func(suppressed map[string]struct{})) error {
	suppressed := make(map[string]struct{})
	for pass := 0; pass < 2; pass++ {
		c.mu.RLock()
		if !gate() {
			c.mu.RUnlock()
			return errDirtyOverlayFallback
		}
		startSeq := c.mutationSeq
		todo := c.dirtyToRefreshLocked(suppressed)
		if len(todo) == 0 {
			// Cache is clean, or every remaining dirty row is a confirmed
			// absence: serve under this same lock hold — but only after
			// re-verifying no suppressed row was resurrected.
			if c.retrySuppressedChurnLocked(suppressed, startSeq) {
				c.mu.RUnlock()
				continue
			}
			collect(suppressed)
			c.mu.RUnlock()
			return nil
		}
		if len(c.dirty) > dirtyOverlayMaxGets {
			c.mu.RUnlock()
			return errDirtyOverlayFallback
		}
		c.mu.RUnlock()

		fetched, err := c.fetchDirtyOverlay(todo, suppressed)
		if err != nil {
			return errDirtyOverlayFallback
		}

		c.mu.Lock()
		if !gate() {
			c.mu.Unlock()
			return errDirtyOverlayFallback
		}
		absorbed := 0
		for _, f := range fetched {
			// Fence: never overwrite a mutation that landed after the snapshot.
			// A skipped-but-still-dirty row is caught by the re-check below and
			// handled by the retry-or-fallback.
			if c.deletedSeq[f.id] > startSeq || c.beadSeq[f.id] > startSeq {
				continue
			}
			c.absorbDirtyOverlayLocked(f.id, f.bead)
			absorbed++
		}
		if absorbed > 0 {
			c.markFreshLocked(time.Now())
			c.updateStatsLocked()
		}
		if len(c.dirtyToRefreshLocked(suppressed)) == 0 {
			if c.retrySuppressedChurnLocked(suppressed, startSeq) {
				c.mu.Unlock()
				continue
			}
			collect(suppressed)
			c.mu.Unlock()
			return nil
		}
		c.mu.Unlock()
	}
	return errDirtyOverlayFallback
}

// dirtyToRefreshLocked returns the dirty IDs still needing a backing refresh:
// every dirty mark not already confirmed absent this read. Caller must hold
// c.mu (read or write).
func (c *CachingStore) dirtyToRefreshLocked(suppressed map[string]struct{}) []string {
	if len(c.dirty) == 0 {
		return nil
	}
	var todo []string
	for id := range c.dirty {
		if _, ok := suppressed[id]; ok {
			continue
		}
		todo = append(todo, id)
	}
	return todo
}

// retrySuppressedChurnLocked guards the serve against a torn read: an
// ErrNotFound-suppressed row re-installed by a concurrent event-apply between
// its fetch and this final lock hold (the symmetric fence to the fetched-row
// deletedSeq/beadSeq check). A suppressed id is churn if its fence advanced past
// the snapshot, or a resident non-dirty row is now present — in either case
// omitting it from collect would serve the cache MINUS a now-live row. Any such
// id is dropped from suppressed so the next pass re-fetches it, and the function
// reports true so the caller retries (or, on the final pass, falls back). Caller
// must hold c.mu. Returns false when the serve may proceed.
func (c *CachingStore) retrySuppressedChurnLocked(suppressed map[string]struct{}, startSeq uint64) bool {
	if len(suppressed) == 0 {
		return false
	}
	var churned []string
	for id := range suppressed {
		if c.beadSeq[id] > startSeq || c.deletedSeq[id] > startSeq {
			churned = append(churned, id)
			continue
		}
		if _, resident := c.beads[id]; resident {
			if _, dirty := c.dirty[id]; !dirty {
				churned = append(churned, id)
			}
		}
	}
	for _, id := range churned {
		delete(suppressed, id)
	}
	return len(churned) > 0
}

// overlayFetched carries a dirty bead freshly read from the backing store,
// queued for absorb under the lock.
type overlayFetched struct {
	id   string
	bead Bead
}

// fetchDirtyOverlay reads each dirty ID via backing.Get with no lock held.
// Successful Gets are queued for absorb; ErrNotFound IDs are added to suppressed
// (their dirty mark is deliberately left set, mirroring Get's dirty path —
// convergence stays with the reconciler). Any other error returns non-nil so
// the caller falls back to the full backing scan.
//
// The fetched bead's own dependency fields are the authoritative source for the
// absorb: our production BdStore.Get carries Needs+Dependencies, and
// depsFromBeadFields reconstructs the dep set from them, so a blocked bead is
// never served as ready. Unlike upstream's DoltLite read store we have no
// dep-stripping backing, so no separate backing.DepList round-trip is needed.
func (c *CachingStore) fetchDirtyOverlay(todo []string, suppressed map[string]struct{}) ([]overlayFetched, error) {
	fetched := make([]overlayFetched, 0, len(todo))
	for _, id := range todo {
		fresh, err := c.backing.Get(id)
		switch {
		case err == nil:
			fetched = append(fetched, overlayFetched{id: id, bead: fresh})
		case errors.Is(err, ErrNotFound):
			suppressed[id] = struct{}{}
		default:
			return nil, err
		}
	}
	return fetched, nil
}

// absorbDirtyOverlayLocked installs a backing-fresh copy of a dirty bead into
// the cache, mirroring the single-ID absorb in Get: it overwrites the cached
// bead and its field-derived deps, then clears the dirty mark and staleness
// fences (beadSeq is cleared but localBeadAt is left in place, matching Get).
// Caller must hold c.mu for writing and must already have applied the
// deletedSeq/beadSeq>startSeq fence for id.
func (c *CachingStore) absorbDirtyOverlayLocked(id string, fresh Bead) {
	c.beads[id] = cloneBead(fresh)
	c.deps[id] = depsFromBeadFields(fresh)
	delete(c.dirty, id)
	delete(c.deletedSeq, id)
	delete(c.beadSeq, id)
}

// Ready returns open beads whose blocking deps are all closed.
func (c *CachingStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	if readyQueryFromArgs(query) != (ReadyQuery{}) {
		return c.backing.Ready(query...)
	}
	var (
		statusByID map[string]string
		depsByID   map[string][]Dep
		openBeads  []Bead
	)
	// Ready requires a fully live cache with complete dependency coverage; the
	// overlay refreshes any dirty rows first, then readiness is computed from
	// the cache. On overlay decline the read takes the old full backing.Ready
	// scan. Refreshing each dirty row's deps (depsFromBeadFields) is what keeps
	// a now-blocked bead from being served as ready.
	if err := c.readCacheWithOverlay(
		func() bool { return c.state == cacheLive && c.depsComplete && c.primePartialErr == nil },
		func(suppressed map[string]struct{}) {
			statusByID = make(map[string]string, len(c.beads))
			openBeads = make([]Bead, 0, len(c.beads))
			for _, b := range c.beads {
				if _, gone := suppressed[b.ID]; gone {
					continue
				}
				statusByID[b.ID] = b.Status
				if b.Status == "open" && !b.Ephemeral && !IsReadyExcludedType(b.Type) {
					openBeads = append(openBeads, cloneBead(b))
				}
			}
			depsByID = make(map[string][]Dep, len(openBeads))
			for _, b := range openBeads {
				depsByID[b.ID] = cloneDeps(c.deps[b.ID])
			}
		},
	); err != nil {
		return c.backing.Ready(query...)
	}

	var result []Bead
	for _, b := range openBeads {
		blocked := false
		for _, dep := range depsByID[b.ID] {
			switch dep.Type {
			case "blocks", "waits-for", "conditional-blocks":
			default:
				continue
			}
			if status, ok := statusByID[dep.DependsOnID]; ok && status != "closed" {
				blocked = true
				break
			}
		}
		if !blocked {
			result = append(result, cloneBead(b))
		}
	}
	return result, nil
}

// CachedReady returns ready beads from the in-memory active read model.
// The boolean reports whether the cache was initialized enough to answer
// without touching the backing store. Unlike Ready, this can answer from a
// partial active cache only when each open bead has known dependency coverage.
func (c *CachingStore) CachedReady() ([]Bead, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.state != cacheLive && c.state != cachePartial {
		return nil, false
	}
	if c.primePartialErr != nil || len(c.dirty) > 0 {
		return nil, false
	}

	statusByID := make(map[string]string, len(c.beads))
	openBeads := make([]Bead, 0, len(c.beads))
	for _, b := range c.beads {
		statusByID[b.ID] = b.Status
		if b.Status == "open" && !b.Ephemeral && !IsReadyExcludedType(b.Type) {
			openBeads = append(openBeads, cloneBead(b))
		}
	}

	result := make([]Bead, 0, len(openBeads))
	for _, b := range openBeads {
		deps, ok := c.deps[b.ID]
		switch {
		case ok:
		case c.depsComplete:
			deps = nil
		default:
			return nil, false
		}
		if cachedBeadReady(statusByID, deps) {
			result = append(result, cloneBead(b))
		}
	}
	return result, true
}

func cachedBeadReady(statusByID map[string]string, deps []Dep) bool {
	for _, dep := range deps {
		switch dep.Type {
		case "blocks", "waits-for", "conditional-blocks":
		default:
			continue
		}
		if status, ok := statusByID[dep.DependsOnID]; ok && status != "closed" {
			return false
		}
	}
	return true
}

// Children returns beads with the given parent ID.
func (c *CachingStore) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	return c.List(ListQuery{
		ParentID:      parentID,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedAsc,
	})
}

// ListByLabel returns beads matching the given label. By default, serves from
// cache only (non-closed beads). Pass IncludeClosed to also query the backing
// store for closed beads and merge results.
func (c *CachingStore) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return c.List(ListQuery{
		Label:         label,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedDesc,
		TierMode:      TierModeFromOpts(opts),
	})
}

// ListByAssignee returns beads assigned to the given agent with matching status.
func (c *CachingStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	return c.List(ListQuery{
		Assignee: assignee,
		Status:   status,
		Limit:    limit,
		Sort:     SortCreatedDesc,
	})
}

// ListByMetadata filters beads by metadata key-value pairs. By default, serves
// from cache only (non-closed beads). Pass IncludeClosed to also query the
// backing store for closed beads and merge results.
func (c *CachingStore) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return c.List(ListQuery{
		Metadata:      filters,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedDesc,
		TierMode:      TierModeFromOpts(opts),
	})
}

func matchesMetadata(b Bead, filters map[string]string) bool {
	for k, v := range filters {
		if b.Metadata[k] != v {
			return false
		}
	}
	return true
}

// DepList returns dependencies for a bead in the given direction.
func (c *CachingStore) DepList(id, direction string) ([]Dep, error) {
	c.mu.RLock()
	if c.state == cacheLive {
		if direction == "down" || direction == "" {
			if !c.depsComplete {
				c.mu.RUnlock()
				return c.backing.DepList(id, direction)
			}
			if deps, ok := c.deps[id]; ok {
				c.mu.RUnlock()
				return cloneDeps(deps), nil
			}
			// Dep not cached yet - fetch from backing and cache it.
			c.mu.RUnlock()
			deps, err := c.backing.DepList(id, direction)
			if err != nil {
				return nil, err
			}
			c.mu.Lock()
			c.deps[id] = cloneDeps(deps)
			c.mu.Unlock()
			return deps, nil
		}
		// Reverse lookups are only partially cached; defer to the backing
		// store so callers do not observe incomplete results.
		c.mu.RUnlock()
		return c.backing.DepList(id, direction)
	}
	c.mu.RUnlock()
	return c.backing.DepList(id, direction)
}

// Ping delegates to the backing store.
func (c *CachingStore) Ping() error {
	return c.backing.Ping()
}
