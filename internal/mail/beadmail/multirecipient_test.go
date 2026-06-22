package beadmail

import (
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// callCountingStore records the store round-trips beadmail issues so tests can
// assert that the multi-recipient inbox path collapses the redundant
// per-recipient queries collectMailMessages would otherwise make. Every bd
// store query is a subprocess round-trip in production, so a lower count is a
// proportional latency win (ga-a60).
type callCountingStore struct {
	*beads.MemStore
	mu                 sync.Mutex
	candidateLists     int // Type=message scoped to a recipient route (legacy per-route fan-out)
	globalMessageLists int // Type=message with no assignee (the single global candidate scan)
	routeMetaLists     int // alias=/session_name= session-route lookups
}

func (s *callCountingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	if query.Type == "message" && query.Assignee != "" {
		s.candidateLists++
	}
	if query.Type == "message" && query.Assignee == "" {
		s.globalMessageLists++
	}
	if _, ok := query.Metadata["alias"]; ok {
		s.routeMetaLists++
	}
	if _, ok := query.Metadata["session_name"]; ok {
		s.routeMetaLists++
	}
	s.mu.Unlock()
	return s.MemStore.List(query)
}

func newRoutedSession(t *testing.T, store beads.Store, alias, sessionName string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        alias,
			"session_name": sessionName,
		},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return b
}

// TestInboxRecipientsMatchesPerRecipientUnion pins that the batched fetch
// returns exactly the deduplicated union of the per-recipient Inbox calls it
// replaces — same messages, no duplicates.
func TestInboxRecipientsMatchesPerRecipientUnion(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)
	sess := newRoutedSession(t, store, "rig/worker", "wf__worker")

	// Two unread messages on different routes of the same session.
	if _, err := p.Send("human", "rig/worker", "", "to alias"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Send("human", sess.ID, "", "to id"); err != nil {
		t.Fatal(err)
	}

	recipients := []string{"rig/worker", sess.ID}

	got, err := p.InboxRecipients(recipients)
	if err != nil {
		t.Fatalf("InboxRecipients: %v", err)
	}

	// Reference: union of per-recipient Inbox calls, deduped by ID.
	wantIDs := map[string]bool{}
	for _, r := range recipients {
		msgs, err := p.Inbox(r)
		if err != nil {
			t.Fatalf("Inbox(%q): %v", r, err)
		}
		for _, m := range msgs {
			wantIDs[m.ID] = true
		}
	}

	gotIDs := map[string]bool{}
	for _, m := range got {
		gotIDs[m.ID] = true
	}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("InboxRecipients returned %d unique messages, want %d", len(gotIDs), len(wantIDs))
	}
	for id := range wantIDs {
		if !gotIDs[id] {
			t.Errorf("InboxRecipients missing message %s present in per-recipient union", id)
		}
	}
	if len(got) != len(gotIDs) {
		t.Errorf("InboxRecipients returned duplicates: %d rows, %d unique", len(got), len(gotIDs))
	}
}

// TestInboxRecipientsExcludesReadAndClosed pins that the batched fetch honors
// the same unread/open filter as the single-recipient Inbox.
func TestInboxRecipientsExcludesReadAndClosed(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)
	sess := newRoutedSession(t, store, "rig/worker", "wf__worker")

	unread, err := p.Send("human", "rig/worker", "", "unread")
	if err != nil {
		t.Fatal(err)
	}
	read, err := p.Send("human", sess.ID, "", "read")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Read(read.ID); err != nil {
		t.Fatalf("Read: %v", err)
	}

	got, err := p.InboxRecipients([]string{"rig/worker", sess.ID})
	if err != nil {
		t.Fatalf("InboxRecipients: %v", err)
	}
	if len(got) != 1 || got[0].ID != unread.ID {
		t.Fatalf("InboxRecipients = %#v, want only unread %s", got, unread.ID)
	}
}

// TestInboxRecipientsEmpty pins that an empty recipient set returns no messages
// rather than falling through to a global type=message scan (mirrors
// CountRecipients(nil)).
func TestInboxRecipientsEmpty(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)
	if _, err := p.Send("human", "mayor", "", "msg"); err != nil {
		t.Fatal(err)
	}
	got, err := p.InboxRecipients(nil)
	if err != nil {
		t.Fatalf("InboxRecipients(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("InboxRecipients(nil) = %d messages, want 0", len(got))
	}
}

// TestInboxRecipientsUsesSingleCandidateScan pins the perf contract: the
// message-candidate scan is a single global Type=message,Status=open list with
// the recipient filter applied in memory, not the former per-route fan-out (one
// List per unique route). Every backing-store query is a bd subprocess
// round-trip, so collapsing the fan-out is the residual inbox N+1 fix (ga-mik1).
func TestInboxRecipientsUsesSingleCandidateScan(t *testing.T) {
	store := &callCountingStore{MemStore: beads.NewMemStore()}
	p := New(store)
	sess := newRoutedSession(t, store, "rig/worker", "wf__worker")
	if _, err := p.Send("human", "rig/worker", "", "hi"); err != nil {
		t.Fatal(err)
	}

	recipients := []string{"rig/worker", sess.ID}
	if _, err := p.InboxRecipients(recipients); err != nil {
		t.Fatalf("InboxRecipients: %v", err)
	}

	// One global candidate scan, zero per-route candidate scans — regardless of
	// how many routes the session expands to.
	if store.globalMessageLists != 1 {
		t.Errorf("global candidate scans = %d, want 1", store.globalMessageLists)
	}
	if store.candidateLists != 0 {
		t.Errorf("per-route candidate scans = %d, want 0 (replaced by the global scan)", store.candidateLists)
	}

	// Sanity: the per-recipient fallback path issues one global scan per Inbox
	// call, so two recipients cost strictly more scans than the batched path.
	store2 := &callCountingStore{MemStore: beads.NewMemStore()}
	p2 := New(store2)
	s2 := newRoutedSession(t, store2, "rig/worker", "wf__worker")
	if _, err := p2.Send("human", "rig/worker", "", "hi"); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"rig/worker", s2.ID} {
		if _, err := p2.Inbox(r); err != nil {
			t.Fatal(err)
		}
	}
	if store2.globalMessageLists <= store.globalMessageLists {
		t.Errorf("per-recipient scans = %d, expected more than batched %d", store2.globalMessageLists, store.globalMessageLists)
	}
}

// TestInboxRoutesSkipsRouteDerivation pins that InboxRoutes issues no
// route-derivation queries: the caller supplies the resolved routes, so the
// provider only runs the single global candidate scan. This is the redundancy
// eliminated when mail-target resolution threads the already-loaded session's
// routes into the inbox fetch (ga-mik1).
func TestInboxRoutesSkipsRouteDerivation(t *testing.T) {
	store := &callCountingStore{MemStore: beads.NewMemStore()}
	p := New(store)
	sess := newRoutedSession(t, store, "rig/worker", "wf__worker")
	if _, err := p.Send("human", "rig/worker", "", "to alias"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Send("human", sess.ID, "", "to id"); err != nil {
		t.Fatal(err)
	}

	got, err := p.InboxRoutes(RoutesForSession(sess))
	if err != nil {
		t.Fatalf("InboxRoutes: %v", err)
	}

	if store.routeMetaLists != 0 {
		t.Errorf("route-derivation metadata lists = %d, want 0 (routes supplied by caller)", store.routeMetaLists)
	}
	if store.globalMessageLists != 1 {
		t.Errorf("global candidate scans = %d, want 1", store.globalMessageLists)
	}
	if store.candidateLists != 0 {
		t.Errorf("per-route candidate scans = %d, want 0", store.candidateLists)
	}
	if len(got) != 2 {
		t.Fatalf("InboxRoutes returned %d messages, want 2 (alias + id routes)", len(got))
	}
}

// TestInboxRoutesMatchesInboxRecipients pins that threading pre-resolved routes
// yields the same unread set as deriving them from the recipient addresses.
func TestInboxRoutesMatchesInboxRecipients(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)
	sess := newRoutedSession(t, store, "rig/worker", "wf__worker")
	if _, err := p.Send("human", "rig/worker", "", "to alias"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Send("human", sess.ID, "", "to id"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Send("human", "wf__worker", "", "to name"); err != nil {
		t.Fatal(err)
	}

	viaRoutes, err := p.InboxRoutes(RoutesForSession(sess))
	if err != nil {
		t.Fatalf("InboxRoutes: %v", err)
	}
	viaRecipients, err := p.InboxRecipients([]string{"rig/worker", sess.ID, "wf__worker"})
	if err != nil {
		t.Fatalf("InboxRecipients: %v", err)
	}

	routeIDs := map[string]bool{}
	for _, m := range viaRoutes {
		routeIDs[m.ID] = true
	}
	if len(viaRoutes) != len(viaRecipients) || len(routeIDs) != len(viaRecipients) {
		t.Fatalf("InboxRoutes returned %d messages, InboxRecipients %d", len(viaRoutes), len(viaRecipients))
	}
	for _, m := range viaRecipients {
		if !routeIDs[m.ID] {
			t.Errorf("InboxRoutes missing %s present in InboxRecipients", m.ID)
		}
	}
}

// TestInboxRoutesEmpty pins that an empty route set returns no messages rather
// than scanning every mailbox (mirrors InboxRecipients(nil)).
func TestInboxRoutesEmpty(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)
	if _, err := p.Send("human", "mayor", "", "msg"); err != nil {
		t.Fatal(err)
	}
	got, err := p.InboxRoutes(nil)
	if err != nil {
		t.Fatalf("InboxRoutes(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("InboxRoutes(nil) = %d messages, want 0", len(got))
	}
}

// TestRoutesForSessionIncludesAllAddresses pins that the threaded route set
// covers every address a session answers to: ID, alias, session name, and
// historical aliases.
func TestRoutesForSessionIncludesAllAddresses(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "rig/worker",
			"session_name":  "wf__worker",
			"alias_history": "rig/old-worker",
		},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	routes := RoutesForSession(b)
	for _, want := range []string{b.ID, "rig/worker", "wf__worker", "rig/old-worker"} {
		if !containsRecipientRoute(routes, want) {
			t.Errorf("RoutesForSession missing %q: %v", want, routes)
		}
	}
}

// TestRecipientRoutesForAllSkipsCoveredRecipients pins that route derivation is
// not repeated for sibling addresses of a session already expanded by an
// earlier recipient.
func TestRecipientRoutesForAllSkipsCoveredRecipients(t *testing.T) {
	store := &callCountingStore{MemStore: beads.NewMemStore()}
	p := New(store)
	sess := newRoutedSession(t, store, "rig/worker", "wf__worker")

	// All three addresses belong to one session, so deriving routes for the
	// first should cover the rest.
	routes := p.recipientRoutesForAll([]string{"rig/worker", sess.ID, "wf__worker"})

	// Route derivation does two metadata lookups (alias=, session_name=) per
	// *uncovered* recipient. With sibling-skip only the first is derived.
	if store.routeMetaLists != 2 {
		t.Errorf("route-derivation metadata lists = %d, want 2 (siblings of one session must be skipped)", store.routeMetaLists)
	}

	// The union must still contain every address of the session.
	for _, want := range []string{"rig/worker", sess.ID, "wf__worker"} {
		if !containsRecipientRoute(routes, want) {
			t.Errorf("route union missing %q: %v", want, routes)
		}
	}
}

// TestRecipientRoutesForAllDerivesDistinctSessions pins that the sibling-skip
// does not collapse genuinely distinct recipients: two unrelated sessions must
// both be derived.
func TestRecipientRoutesForAllDerivesDistinctSessions(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)
	a := newRoutedSession(t, store, "rig/a", "wf__a")
	b := newRoutedSession(t, store, "rig/b", "wf__b")

	routes := p.recipientRoutesForAll([]string{"rig/a", "rig/b"})

	for _, want := range []string{"rig/a", a.ID, "wf__a", "rig/b", b.ID, "wf__b"} {
		if !containsRecipientRoute(routes, want) {
			t.Errorf("route union missing %q: %v", want, routes)
		}
	}
}
