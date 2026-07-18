package api

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

type peekOnlyHandle struct {
	output string
}

func (h peekOnlyHandle) Peek(context.Context, int) (string, error) {
	return h.output, nil
}

func TestStreamSessionPeekAcceptsPeekCapability(t *testing.T) {
	srv := New(newSessionFakeState(t))
	info := session.Info{ID: "sess-1", Template: "probe", Provider: "claude"}
	rec := newSyncResponseRecorder()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		srv.streamSessionPeek(ctx, rec, info, peekOnlyHandle{output: "hello from peek"})
		close(done)
	}()

	waitForRecorderSubstring(t, rec, "hello from peek", 250*time.Millisecond)
	cancel()
	<-done

	// Read the final body only after the streaming goroutine has returned,
	// so the recorder is no longer being written concurrently.
	body := rec.BodyString()
	if !strings.Contains(body, "hello from peek") {
		t.Fatalf("stream body missing peek output: %s", body)
	}
	if !strings.Contains(body, `"provider":"claude"`) {
		t.Fatalf("stream body missing provider envelope: %s", body)
	}
}

type peekPendingHandle struct {
	mu      sync.Mutex
	output  string
	pending *worker.PendingInteraction
}

func (h *peekPendingHandle) Peek(context.Context, int) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.output, nil
}

func (h *peekPendingHandle) Pending(context.Context) (*worker.PendingInteraction, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending == nil {
		return nil, nil
	}
	copyPending := *h.pending
	copyPending.Options = append([]string(nil), h.pending.Options...)
	copyPending.Metadata = cloneStringMap(h.pending.Metadata)
	return &copyPending, nil
}

func (h *peekPendingHandle) PendingStatus(ctx context.Context) (*worker.PendingInteraction, bool, error) {
	pending, err := h.Pending(ctx)
	return pending, true, err
}

func (h *peekPendingHandle) Respond(context.Context, worker.InteractionResponse) error {
	return nil
}

func (h *peekPendingHandle) SetPending(pending *worker.PendingInteraction) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if pending == nil {
		h.pending = nil
		return
	}
	copyPending := *pending
	copyPending.Options = append([]string(nil), pending.Options...)
	copyPending.Metadata = cloneStringMap(pending.Metadata)
	h.pending = &copyPending
}

func TestStreamSessionPeekRawWorkerWakeEmitsPendingWithoutOutputChange(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	info := session.Info{ID: "sess-1", Template: "probe", Provider: "claude"}
	handle := &peekPendingHandle{output: "steady output"}
	rec := newSyncResponseRecorder()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		srv.streamSessionPeekRaw(ctx, rec, info, handle)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "steady output", time.Second); !strings.Contains(body, "steady output") {
		t.Fatalf("stream body missing initial peek output: %s", body)
	} else if !strings.Contains(body, `"provider":"claude"`) {
		t.Fatalf("stream body missing provider envelope: %s", body)
	}

	handle.SetPending(&worker.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "Proceed?",
	})
	fs.eventProv.(*events.Fake).Record(events.Event{
		Type:    events.WorkerOperation,
		Actor:   "worker",
		Subject: info.ID,
	})

	body := waitForRecorderSubstring(t, rec, "req-1", 1500*time.Millisecond)

	cancel()
	<-done

	if !strings.Contains(body, "req-1") {
		t.Fatalf("raw stream body missing pending interaction after worker wake: %s", body)
	}
}

var _ worker.PeekHandle = peekOnlyHandle{}
