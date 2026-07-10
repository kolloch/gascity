package beads

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
)

// countingDepsStore mirrors production BdStore semantics for the dirty-overlay
// tests: Get returns each bead with its Dependencies field populated from the
// store's dep edges (MemStore.Get alone omits them, keeping deps in a side
// table), so the overlay's depsFromBeadFields refresh reconstructs the
// authoritative dep set. It counts backing Get/List/Ready calls so a test can
// prove the overlay served from cache instead of falling back, and can force a
// per-ID Get error.
type countingDepsStore struct {
	*MemStore
	mu         sync.Mutex
	getCalls   int
	listCalls  int
	readyCalls int
	getErr     map[string]error
}

func newCountingDepsStore() *countingDepsStore {
	return &countingDepsStore{MemStore: NewMemStore(), getErr: map[string]error{}}
}

func (s *countingDepsStore) Get(id string) (Bead, error) {
	s.mu.Lock()
	s.getCalls++
	err := s.getErr[id]
	s.mu.Unlock()
	if err != nil {
		return Bead{}, err
	}
	b, err := s.MemStore.Get(id)
	if err != nil {
		return Bead{}, err
	}
	deps, err := s.DepList(id, "down")
	if err != nil {
		return Bead{}, err
	}
	b.Dependencies = deps
	return b, nil
}

func (s *countingDepsStore) List(query ListQuery) ([]Bead, error) {
	s.mu.Lock()
	s.listCalls++
	s.mu.Unlock()
	return s.MemStore.List(query)
}

func (s *countingDepsStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	s.mu.Lock()
	s.readyCalls++
	s.mu.Unlock()
	return s.MemStore.Ready(query...)
}

func (s *countingDepsStore) counts() (get, list, ready int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getCalls, s.listCalls, s.readyCalls
}

func (s *countingDepsStore) setGetErr(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getErr[id] = err
}

// markDirtyForTest sets the dirty mark on ids directly, mimicking the write
// path's post-failure "couldn't refresh, mark stale" branch without needing a
// failing backing.
func markDirtyForTest(c *CachingStore, ids ...string) {
	c.mu.Lock()
	for _, id := range ids {
		c.dirty[id] = struct{}{}
	}
	c.mu.Unlock()
}

type beadView struct {
	ID, Title, Status, Type, ParentID, Assignee string
}

func viewsOf(beads []Bead) []beadView {
	views := make([]beadView, len(beads))
	for i, b := range beads {
		views[i] = beadView{
			ID:       b.ID,
			Title:    b.Title,
			Status:   b.Status,
			Type:     b.Type,
			ParentID: b.ParentID,
			Assignee: b.Assignee,
		}
	}
	return views
}

func sortedIDs(beads []Bead) []string {
	ids := make([]string, len(beads))
	for i, b := range beads {
		ids[i] = b.ID
	}
	sort.Strings(ids)
	return ids
}

func hasID(beads []Bead, id string) bool {
	for _, b := range beads {
		if b.ID == id {
			return true
		}
	}
	return false
}

// TestCachingStoreDirtyOverlayServesListFromCache proves the core amplifier fix:
// with a dirty set at the cap, List refreshes only the dirty rows via
// backing.Get and serves from cache — it does NOT decline the whole cache to a
// full backing.List scan — and the result is byte-equivalent (rows, order,
// refreshed titles) to the old full-scan path.
func TestCachingStoreDirtyOverlayServesListFromCache(t *testing.T) {
	t.Parallel()

	backing := newCountingDepsStore()
	var ids []string
	for i := 0; i < 12; i++ {
		b, err := backing.Create(Bead{Title: fmt.Sprintf("bead-%02d", i)})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids = append(ids, b.ID)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// Make exactly cap rows stale in the backing and mark them dirty, so the
	// overlay must refresh precisely those rows (|dirty| == cap, the boundary).
	dirty := ids[:dirtyOverlayMaxGets]
	for i, id := range dirty {
		title := fmt.Sprintf("refreshed-%02d", i)
		if err := backing.Update(id, UpdateOpts{Title: &title}); err != nil {
			t.Fatalf("backing Update: %v", err)
		}
	}
	markDirtyForTest(cache, dirty...)

	query := ListQuery{Status: "open", AllowScan: true, Sort: SortCreatedAsc}
	// The authoritative "old full-scan" answer (does not touch the counters).
	want, err := backing.MemStore.List(liveListQuery(query))
	if err != nil {
		t.Fatalf("backing List: %v", err)
	}

	getBefore, listBefore, _ := backing.counts()
	got, err := cache.List(query)
	if err != nil {
		t.Fatalf("cache List: %v", err)
	}
	getAfter, listAfter, _ := backing.counts()

	if !reflect.DeepEqual(viewsOf(got), viewsOf(want)) {
		t.Fatalf("overlay List = %+v,\nwant byte-equivalent full scan %+v", viewsOf(got), viewsOf(want))
	}
	if listAfter != listBefore {
		t.Fatalf("overlay declined to backing.List (%d calls); want served from cache", listAfter-listBefore)
	}
	if got, want := getAfter-getBefore, len(dirty); got != want {
		t.Fatalf("overlay backing.Get calls = %d, want %d (one per dirty row)", got, want)
	}

	// The overlay absorbed the refreshed rows: the cache is now clean, so a
	// second read touches neither backing.List nor backing.Get.
	getMid, listMid, _ := backing.counts()
	if _, err := cache.List(query); err != nil {
		t.Fatalf("second cache List: %v", err)
	}
	getEnd, listEnd, _ := backing.counts()
	if getEnd != getMid || listEnd != listMid {
		t.Fatalf("second List touched backing (get %d->%d, list %d->%d); want fully cache-served",
			getMid, getEnd, listMid, listEnd)
	}
}

// TestCachingStoreDirtyOverlayServesReadyFromCache is the Ready analog: a dirty
// set at the cap is refreshed per-ID rather than declining to backing.Ready,
// and the ready set matches the old full-scan.
func TestCachingStoreDirtyOverlayServesReadyFromCache(t *testing.T) {
	t.Parallel()

	backing := newCountingDepsStore()
	var ids []string
	for i := 0; i < 10; i++ {
		b, err := backing.Create(Bead{Title: fmt.Sprintf("bead-%02d", i)})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids = append(ids, b.ID)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	dirty := ids[:dirtyOverlayMaxGets]
	markDirtyForTest(cache, dirty...)

	want, err := backing.MemStore.Ready()
	if err != nil {
		t.Fatalf("backing Ready: %v", err)
	}

	getBefore, _, readyBefore := backing.counts()
	got, err := cache.Ready()
	if err != nil {
		t.Fatalf("cache Ready: %v", err)
	}
	getAfter, _, readyAfter := backing.counts()

	if !reflect.DeepEqual(sortedIDs(got), sortedIDs(want)) {
		t.Fatalf("overlay Ready ids = %v, want full-scan ids %v", sortedIDs(got), sortedIDs(want))
	}
	if readyAfter != readyBefore {
		t.Fatalf("overlay declined to backing.Ready (%d calls); want served from cache", readyAfter-readyBefore)
	}
	if got, want := getAfter-getBefore, len(dirty); got != want {
		t.Fatalf("overlay backing.Get calls = %d, want %d (one per dirty row)", got, want)
	}
}

// TestCachingStoreDirtyOverlayFallsBackOverCap proves the bounded-cap guard:
// with more dirty rows than the cap, both List and Ready decline the overlay
// and fall back to the full backing scan WITHOUT issuing any per-ID Gets — the
// degrade-to-prior-behavior safety valve.
func TestCachingStoreDirtyOverlayFallsBackOverCap(t *testing.T) {
	t.Parallel()

	backing := newCountingDepsStore()
	var ids []string
	for i := 0; i < dirtyOverlayMaxGets+4; i++ {
		b, err := backing.Create(Bead{Title: fmt.Sprintf("bead-%02d", i)})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids = append(ids, b.ID)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// |dirty| == cap+1 > cap, so the overlay declines before fetching.
	dirty := ids[:dirtyOverlayMaxGets+1]
	markDirtyForTest(cache, dirty...)

	query := ListQuery{Status: "open", AllowScan: true, Sort: SortCreatedAsc}
	wantList, err := backing.MemStore.List(liveListQuery(query))
	if err != nil {
		t.Fatalf("backing List: %v", err)
	}

	getBefore, listBefore, readyBefore := backing.counts()
	gotList, err := cache.List(query)
	if err != nil {
		t.Fatalf("cache List: %v", err)
	}
	gotReady, err := cache.Ready()
	if err != nil {
		t.Fatalf("cache Ready: %v", err)
	}
	getAfter, listAfter, readyAfter := backing.counts()

	if !reflect.DeepEqual(viewsOf(gotList), viewsOf(wantList)) {
		t.Fatalf("over-cap List = %+v, want backing fallback %+v", viewsOf(gotList), viewsOf(wantList))
	}
	if listAfter-listBefore != 1 {
		t.Fatalf("over-cap List backing.List calls = %d, want 1 (fallback)", listAfter-listBefore)
	}
	if readyAfter-readyBefore != 1 {
		t.Fatalf("over-cap Ready backing.Ready calls = %d, want 1 (fallback)", readyAfter-readyBefore)
	}
	if getAfter != getBefore {
		t.Fatalf("over-cap path issued %d backing.Get calls; want 0 (cap declines before fetch)", getAfter-getBefore)
	}
	// Ready fallback returns the authoritative backing set.
	wantReady, err := backing.MemStore.Ready()
	if err != nil {
		t.Fatalf("backing Ready: %v", err)
	}
	if !reflect.DeepEqual(sortedIDs(gotReady), sortedIDs(wantReady)) {
		t.Fatalf("over-cap Ready ids = %v, want backing fallback %v", sortedIDs(gotReady), sortedIDs(wantReady))
	}
}

// TestCachingStoreDirtyOverlayReadyReflectsNewlyBlockedRow is the Ready
// correctness invariant (the R1 analog): a dirty row whose backing refresh adds
// a blocking dependency must NOT be served as ready. Without the per-ID deps
// refresh the stale cache would serve it as ready.
func TestCachingStoreDirtyOverlayReadyReflectsNewlyBlockedRow(t *testing.T) {
	t.Parallel()

	backing := newCountingDepsStore()
	blocker, err := backing.Create(Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create(blocker): %v", err)
	}
	target, err := backing.Create(Bead{Title: "target"})
	if err != nil {
		t.Fatalf("Create(target): %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// Clean-cache sanity: target has no deps, so it is ready.
	ready0, err := cache.Ready()
	if err != nil {
		t.Fatalf("cache Ready (clean): %v", err)
	}
	if !hasID(ready0, target.ID) {
		t.Fatalf("target %s not ready in clean cache: %v", target.ID, sortedIDs(ready0))
	}

	// Backing gains a blocking edge target -> blocker (blocker still open);
	// the cache is stale until target is refreshed.
	if err := backing.DepAdd(target.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	markDirtyForTest(cache, target.ID)

	_, _, readyBefore := backing.counts()
	ready1, err := cache.Ready()
	if err != nil {
		t.Fatalf("cache Ready (stale): %v", err)
	}
	_, _, readyAfter := backing.counts()

	if hasID(ready1, target.ID) {
		t.Fatalf("target %s served as ready after gaining an open blocker: %v", target.ID, sortedIDs(ready1))
	}
	if readyAfter != readyBefore {
		t.Fatalf("Ready declined to backing (%d calls); the overlay must recompute readiness from cache", readyAfter-readyBefore)
	}
}

// TestCachingStoreDirtyOverlayReadyReflectsUnblockedRow is the opposite
// direction: a dirty blocker that closes in the backing must unblock its
// dependent, which then appears in Ready.
func TestCachingStoreDirtyOverlayReadyReflectsUnblockedRow(t *testing.T) {
	t.Parallel()

	backing := newCountingDepsStore()
	blocker, err := backing.Create(Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create(blocker): %v", err)
	}
	target, err := backing.Create(Bead{Title: "target", Needs: []string{blocker.ID}})
	if err != nil {
		t.Fatalf("Create(target): %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	ready0, err := cache.Ready()
	if err != nil {
		t.Fatalf("cache Ready (clean): %v", err)
	}
	if hasID(ready0, target.ID) {
		t.Fatalf("target %s ready while blocked by open blocker: %v", target.ID, sortedIDs(ready0))
	}

	// Blocker closes in the backing; refreshing the dirty blocker unblocks target.
	if err := backing.Close(blocker.ID); err != nil {
		t.Fatalf("Close(blocker): %v", err)
	}
	markDirtyForTest(cache, blocker.ID)

	ready1, err := cache.Ready()
	if err != nil {
		t.Fatalf("cache Ready (stale): %v", err)
	}
	if !hasID(ready1, target.ID) {
		t.Fatalf("target %s not ready after blocker closed: %v", target.ID, sortedIDs(ready1))
	}
}

// TestCachingStoreDirtyOverlaySuppressesDeletedRow proves the suppressed-set
// path: a dirty row deleted in the backing (Get -> ErrNotFound) is omitted from
// the served result, matching what the old full backing.List would return,
// while the surviving row is served from cache.
func TestCachingStoreDirtyOverlaySuppressesDeletedRow(t *testing.T) {
	t.Parallel()

	backing := newCountingDepsStore()
	keep, err := backing.Create(Bead{Title: "keep"})
	if err != nil {
		t.Fatalf("Create(keep): %v", err)
	}
	gone, err := backing.Create(Bead{Title: "gone"})
	if err != nil {
		t.Fatalf("Create(gone): %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := backing.Delete(gone.ID); err != nil {
		t.Fatalf("Delete(gone): %v", err)
	}
	markDirtyForTest(cache, gone.ID)

	query := ListQuery{Status: "open", AllowScan: true, Sort: SortCreatedAsc}
	_, listBefore, _ := backing.counts()
	got, err := cache.List(query)
	if err != nil {
		t.Fatalf("cache List: %v", err)
	}
	_, listAfter, _ := backing.counts()

	if hasID(got, gone.ID) {
		t.Fatalf("deleted dirty row %s served in List: %v", gone.ID, sortedIDs(got))
	}
	if !hasID(got, keep.ID) {
		t.Fatalf("surviving row %s missing from List: %v", keep.ID, sortedIDs(got))
	}
	if listAfter != listBefore {
		t.Fatalf("suppressed-row path declined to backing.List (%d calls); want served from cache", listAfter-listBefore)
	}
}

// TestCachingStoreDirtyOverlayFallsBackOnGetError proves a non-NotFound
// backing.Get failure declines the overlay and takes the old full-scan
// fallback rather than serving a stale or partial cache.
func TestCachingStoreDirtyOverlayFallsBackOnGetError(t *testing.T) {
	t.Parallel()

	backing := newCountingDepsStore()
	stable, err := backing.Create(Bead{Title: "stable"})
	if err != nil {
		t.Fatalf("Create(stable): %v", err)
	}
	flaky, err := backing.Create(Bead{Title: "flaky"})
	if err != nil {
		t.Fatalf("Create(flaky): %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.setGetErr(flaky.ID, errors.New("transient backing failure"))
	markDirtyForTest(cache, flaky.ID)

	query := ListQuery{Status: "open", AllowScan: true, Sort: SortCreatedAsc}
	_, listBefore, _ := backing.counts()
	got, err := cache.List(query)
	if err != nil {
		t.Fatalf("cache List: %v", err)
	}
	_, listAfter, _ := backing.counts()

	if listAfter-listBefore != 1 {
		t.Fatalf("Get-error path backing.List calls = %d, want 1 (fallback)", listAfter-listBefore)
	}
	if !hasID(got, stable.ID) || !hasID(got, flaky.ID) {
		t.Fatalf("fallback List missing rows: %v", sortedIDs(got))
	}
}

// TestCachingStoreDirtyOverlayRaceClean stresses the lock-drop / re-fetch /
// re-lock overlay path with concurrent dirty marking and backing mutation.
// Run with -race to catch data races; the assertion is simply that reads never
// error under churn.
func TestCachingStoreDirtyOverlayRaceClean(t *testing.T) {
	t.Parallel()

	backing := newCountingDepsStore()
	var ids []string
	for i := 0; i < 24; i++ {
		b, err := backing.Create(Bead{Title: fmt.Sprintf("b-%02d", i)})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids = append(ids, b.ID)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	stop := make(chan struct{})
	var dirtier sync.WaitGroup
	dirtier.Add(1)
	go func() {
		defer dirtier.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			id := ids[i%len(ids)]
			title := fmt.Sprintf("b-%02d-r%d", i%len(ids), i)
			_ = backing.Update(id, UpdateOpts{Title: &title})
			cache.mu.Lock()
			if len(cache.dirty) < dirtyOverlayMaxGets {
				cache.dirty[id] = struct{}{}
			}
			cache.mu.Unlock()
		}
	}()

	var readers sync.WaitGroup
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 300; j++ {
				if _, err := cache.List(ListQuery{Status: "open", AllowScan: true}); err != nil {
					t.Errorf("List under churn: %v", err)
					return
				}
				if _, err := cache.Ready(); err != nil {
					t.Errorf("Ready under churn: %v", err)
					return
				}
			}
		}()
	}

	readers.Wait()
	close(stop)
	dirtier.Wait()
}
