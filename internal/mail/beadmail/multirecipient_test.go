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
	mu             sync.Mutex
	candidateLists int // Type=message scoped to a recipient route
	routeMetaLists int // alias=/session_name= session-route lookups
}

func (s *callCountingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	if query.Type == "message" && query.Assignee != "" {
		s.candidateLists++
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

// TestInboxRecipientsScansEachRouteOnce pins the perf contract: the message
// candidate scan runs once per *unique* route, not once per route per
// recipient as the per-recipient loop did.
func TestInboxRecipientsScansEachRouteOnce(t *testing.T) {
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

	// The session expands to exactly three routes (id, alias, session_name);
	// the candidate scan must run once per unique route.
	const wantRoutes = 3
	if store.candidateLists != wantRoutes {
		t.Errorf("candidate scans = %d, want %d (one per unique route)", store.candidateLists, wantRoutes)
	}

	// Sanity: the old per-recipient path scans the routes once per recipient,
	// so it must issue strictly more candidate scans than the batched path.
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
	if store2.candidateLists <= store.candidateLists {
		t.Errorf("per-recipient candidate scans = %d, expected more than batched %d", store2.candidateLists, store.candidateLists)
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
