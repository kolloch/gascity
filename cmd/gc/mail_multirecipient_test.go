package main

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
)

// beadmail must keep implementing the batched fetch fast-path so inbox/check
// avoid per-recipient re-querying (ga-a60). A compile error here means the
// fast-path silently degraded to the per-recipient fallback.
var _ multiRecipientMailFetcher = (*beadmail.Provider)(nil)

// beadmail must also implement the route fast-path so resolved targets skip
// route re-derivation on the inbox/check poll (ga-mik1). A compile error here
// means the threaded-routes optimization silently degraded.
var _ routeMailFetcher = (*beadmail.Provider)(nil)

// routeFetcherSpy implements both fetcher interfaces, recording which path
// collectUnreadMail took so tests can prove the route fast-path wins when the
// target carries pre-resolved routes.
type routeFetcherSpy struct {
	mail.Provider
	routeCalled bool
	recipCalled bool
	gotRoutes   []string
	ret         []mail.Message
	retErr      error
}

func (f *routeFetcherSpy) InboxRoutes(routes []string) ([]mail.Message, error) {
	f.routeCalled = true
	f.gotRoutes = routes
	return f.ret, f.retErr
}

func (f *routeFetcherSpy) InboxRecipients(_ []string) ([]mail.Message, error) {
	f.recipCalled = true
	return f.ret, f.retErr
}

// TestCollectUnreadMailPrefersRouteFastPath pins that a target carrying
// pre-resolved routes is served by InboxRoutes (no route re-derivation), not
// InboxRecipients, and that the result is sorted oldest-first.
func TestCollectUnreadMailPrefersRouteFastPath(t *testing.T) {
	older := time.Unix(100, 0)
	newer := time.Unix(200, 0)
	spy := &routeFetcherSpy{
		Provider: mail.NewFake(),
		ret:      []mail.Message{{ID: "m-new", CreatedAt: newer}, {ID: "m-old", CreatedAt: older}},
	}
	target := resolvedMailTarget{
		display:    "rig/worker",
		recipients: []string{"rig/worker", "bd-1"},
		routes:     []string{"rig/worker", "bd-1", "wf__worker"},
	}

	got, err := collectUnreadMail(spy, nil, target)
	if err != nil {
		t.Fatalf("collectUnreadMail: %v", err)
	}
	if !spy.routeCalled {
		t.Error("InboxRoutes was not called — route fast-path not taken")
	}
	if spy.recipCalled {
		t.Error("InboxRecipients was called despite pre-resolved routes")
	}
	if len(spy.gotRoutes) != 3 || spy.gotRoutes[2] != "wf__worker" {
		t.Errorf("InboxRoutes got %v, want the target's 3 routes", spy.gotRoutes)
	}
	if len(got) != 2 || got[0].ID != "m-old" || got[1].ID != "m-new" {
		t.Errorf("collectUnreadMail = %v, want oldest-first [m-old m-new]", got)
	}
}

// TestCollectUnreadMailRouteFallsBackToRecipients pins that a target without
// pre-resolved routes still uses the recipient batched fast-path.
func TestCollectUnreadMailRouteFallsBackToRecipients(t *testing.T) {
	spy := &routeFetcherSpy{Provider: mail.NewFake()}
	target := resolvedMailTarget{recipients: []string{"rig/worker"}} // no routes

	if _, err := collectUnreadMail(spy, nil, target); err != nil {
		t.Fatalf("collectUnreadMail: %v", err)
	}
	if spy.routeCalled {
		t.Error("InboxRoutes was called despite empty target.routes")
	}
	if !spy.recipCalled {
		t.Error("InboxRecipients was not called — recipient fast-path not taken")
	}
}

// fetcherSpy is a mail.Provider that also implements multiRecipientMailFetcher,
// recording the batched call so tests can prove collectUnreadMail prefers it.
type fetcherSpy struct {
	mail.Provider
	called    bool
	gotRecips []string
	ret       []mail.Message
	retErr    error
}

func (f *fetcherSpy) InboxRecipients(recipients []string) ([]mail.Message, error) {
	f.called = true
	f.gotRecips = recipients
	return f.ret, f.retErr
}

// TestCollectUnreadMailUsesFetcherFastPath pins that a provider implementing
// multiRecipientMailFetcher is served by the batched query, not the
// per-recipient fetch, and that the batched result is sorted oldest-first.
func TestCollectUnreadMailUsesFetcherFastPath(t *testing.T) {
	older := time.Unix(100, 0)
	newer := time.Unix(200, 0)
	spy := &fetcherSpy{
		Provider: mail.NewFake(),
		// Returned out of order to prove collectUnreadMail re-sorts.
		ret: []mail.Message{{ID: "m-new", CreatedAt: newer}, {ID: "m-old", CreatedAt: older}},
	}

	fetchCalled := false
	fetch := func(string) ([]mail.Message, error) {
		fetchCalled = true
		return nil, nil
	}

	got, err := collectUnreadMail(spy, fetch, resolvedMailTarget{recipients: []string{"rig/worker", "bd-1"}})
	if err != nil {
		t.Fatalf("collectUnreadMail: %v", err)
	}
	if !spy.called {
		t.Error("InboxRecipients was not called — batched fast-path not taken")
	}
	if fetchCalled {
		t.Error("per-recipient fetch was called despite the batched fast-path")
	}
	if len(spy.gotRecips) != 2 || spy.gotRecips[0] != "rig/worker" || spy.gotRecips[1] != "bd-1" {
		t.Errorf("InboxRecipients got %v, want [rig/worker bd-1]", spy.gotRecips)
	}
	if len(got) != 2 || got[0].ID != "m-old" || got[1].ID != "m-new" {
		t.Errorf("collectUnreadMail = %v, want oldest-first [m-old m-new]", got)
	}
}

// TestCollectUnreadMailPropagatesFetcherError pins that a batched-path error is
// surfaced, not swallowed.
func TestCollectUnreadMailPropagatesFetcherError(t *testing.T) {
	spy := &fetcherSpy{Provider: mail.NewFake(), retErr: errors.New("boom")}
	if _, err := collectUnreadMail(spy, nil, resolvedMailTarget{recipients: []string{"a"}}); err == nil {
		t.Fatal("collectUnreadMail returned nil error, want the fetcher error propagated")
	}
}

// TestCollectUnreadMailFallsBackWithoutFetcher pins that a provider lacking the
// batched method (e.g. the fake/exec providers) still works via the
// per-recipient fetch.
func TestCollectUnreadMailFallsBackWithoutFetcher(t *testing.T) {
	if _, ok := mail.Provider(mail.NewFake()).(multiRecipientMailFetcher); ok {
		t.Skip("fake provider unexpectedly implements multiRecipientMailFetcher")
	}

	fetchCalls := 0
	fetch := func(recipient string) ([]mail.Message, error) {
		fetchCalls++
		return []mail.Message{{ID: "m-" + recipient, CreatedAt: time.Unix(int64(fetchCalls), 0)}}, nil
	}

	got, err := collectUnreadMail(mail.NewFake(), fetch, resolvedMailTarget{recipients: []string{"a", "b"}})
	if err != nil {
		t.Fatalf("collectUnreadMail: %v", err)
	}
	if fetchCalls != 2 {
		t.Errorf("per-recipient fetch calls = %d, want 2 (fallback path)", fetchCalls)
	}
	if len(got) != 2 {
		t.Errorf("collectUnreadMail = %d messages, want 2", len(got))
	}
}
