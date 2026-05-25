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

	got, err := collectUnreadMail(spy, fetch, []string{"rig/worker", "bd-1"})
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
	if _, err := collectUnreadMail(spy, nil, []string{"a"}); err == nil {
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

	got, err := collectUnreadMail(mail.NewFake(), fetch, []string{"a", "b"})
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
