package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func lastSendKeysMessage(f *runtime.Fake, name string) (string, bool) {
	for i := len(f.Calls) - 1; i >= 0; i-- {
		if f.Calls[i].Method == "SendKeys" && f.Calls[i].Name == name {
			return f.Calls[i].Message, true
		}
	}
	return "", false
}

func TestCollectUnstickResults_ParkedSubmitsEnter(t *testing.T) {
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "s-mayor", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.ParkedInputResults["s-mayor"] = true

	targets := []unstickTarget{{id: "gc-1", display: "mayor", sessionName: "s-mayor"}}
	results, err := collectUnstickResults(fake, targets, false)
	if err != nil {
		t.Fatalf("collectUnstickResults: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	r := results[0]
	if !r.Running || !r.Parked || !r.Submitted || r.Error != "" {
		t.Fatalf("result = %+v, want running+parked+submitted, no error", r)
	}
	msg, ok := lastSendKeysMessage(fake, "s-mayor")
	if !ok {
		t.Fatal("expected a SendKeys call for s-mayor")
	}
	if msg != "Enter" {
		t.Errorf("SendKeys keys = %q, want %q", msg, "Enter")
	}
}

func TestCollectUnstickResults_DryRunDoesNotSubmit(t *testing.T) {
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "s-mayor", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.ParkedInputResults["s-mayor"] = true

	results, err := collectUnstickResults(fake, []unstickTarget{{id: "gc-1", display: "mayor", sessionName: "s-mayor"}}, true)
	if err != nil {
		t.Fatalf("collectUnstickResults: %v", err)
	}
	r := results[0]
	if !r.Parked || r.Submitted {
		t.Fatalf("result = %+v, want parked but not submitted", r)
	}
	if n := fake.CountCalls("SendKeys", "s-mayor"); n != 0 {
		t.Errorf("SendKeys calls = %d, want 0 in dry-run", n)
	}
}

func TestCollectUnstickResults_NotParkedDoesNothing(t *testing.T) {
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "s-witness", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.ParkedInputResults["s-witness"] = false

	results, err := collectUnstickResults(fake, []unstickTarget{{id: "gc-2", display: "witness", sessionName: "s-witness"}}, false)
	if err != nil {
		t.Fatalf("collectUnstickResults: %v", err)
	}
	r := results[0]
	if !r.Running || r.Parked || r.Submitted {
		t.Fatalf("result = %+v, want running, not parked, not submitted", r)
	}
	if n := fake.CountCalls("SendKeys", "s-witness"); n != 0 {
		t.Errorf("SendKeys calls = %d, want 0 when not parked", n)
	}
}

func TestCollectUnstickResults_NotRunningSkipped(t *testing.T) {
	fake := runtime.NewFake() // session never started → not running
	results, err := collectUnstickResults(fake, []unstickTarget{{id: "gc-3", display: "nux", sessionName: "s-nux"}}, false)
	if err != nil {
		t.Fatalf("collectUnstickResults: %v", err)
	}
	r := results[0]
	if r.Running || r.Parked || r.Submitted {
		t.Fatalf("result = %+v, want not running", r)
	}
	if n := fake.CountCalls("ParkedInput", "s-nux"); n != 0 {
		t.Errorf("ParkedInput calls = %d, want 0 for a stopped session", n)
	}
}

func TestCollectUnstickResults_ParkedInputErrorRecorded(t *testing.T) {
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "s-x", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.ParkedInputErrors["s-x"] = errors.New("capture failed")

	results, err := collectUnstickResults(fake, []unstickTarget{{id: "gc-4", display: "x", sessionName: "s-x"}}, false)
	if err != nil {
		t.Fatalf("collectUnstickResults: %v", err)
	}
	r := results[0]
	if !r.Running || r.Submitted || r.Error == "" {
		t.Fatalf("result = %+v, want running with error, not submitted", r)
	}
	if n := fake.CountCalls("SendKeys", "s-x"); n != 0 {
		t.Errorf("SendKeys calls = %d, want 0 after detection error", n)
	}
}

// noParkProvider is a runtime.Provider whose dynamic type does NOT implement
// runtime.ParkedInputProvider (the embedded interface promotes only Provider's
// methods), so collectUnstickResults must fail fast.
type noParkProvider struct{ runtime.Provider }

func TestCollectUnstickResults_UnsupportedProvider(t *testing.T) {
	sp := noParkProvider{runtime.NewFake()}
	if _, err := collectUnstickResults(sp, []unstickTarget{{id: "gc-5", sessionName: "s"}}, false); err == nil {
		t.Fatal("expected error when provider cannot detect parked input")
	}
}

func TestUnstickAllTargets_OnlyRunningSessions(t *testing.T) {
	store := beads.NewMemStore()
	mk := func(name, alias string) {
		if _, err := store.Create(beads.Bead{
			Title:  alias,
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"session_name": name,
				"alias":        alias,
			},
		}); err != nil {
			t.Fatalf("store.Create(%s): %v", alias, err)
		}
	}
	mk("s-running", "mayor")
	mk("s-stopped", "witness")

	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "s-running", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	targets, err := unstickAllTargets(store, fake)
	if err != nil {
		t.Fatalf("unstickAllTargets: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %d (%+v), want 1 (only running)", len(targets), targets)
	}
	if targets[0].sessionName != "s-running" || targets[0].display != "mayor" {
		t.Errorf("target = %+v, want s-running/mayor", targets[0])
	}
}

func TestCmdSessionUnstick_ArgValidation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cmdSessionUnstick([]string{"mayor"}, true, false, false, &stdout, &stderr); code != 1 {
		t.Fatalf("--all with arg: code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "cannot combine") {
		t.Errorf("stderr = %q, want mention of cannot combine", stderr.String())
	}

	stderr.Reset()
	if code := cmdSessionUnstick(nil, false, false, false, &stdout, &stderr); code != 1 {
		t.Fatalf("no arg, no --all: code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "specify a session") {
		t.Errorf("stderr = %q, want mention of specify a session", stderr.String())
	}
}

func TestUnstickResultStatus(t *testing.T) {
	tests := []struct {
		name   string
		result sessionUnstickResult
		dryRun bool
		want   string
	}{
		{"error", sessionUnstickResult{Running: true, Error: "boom"}, false, "error: boom"},
		{"not running", sessionUnstickResult{Running: false}, false, "not running — skipped"},
		{"submitted", sessionUnstickResult{Running: true, Parked: true, Submitted: true}, false, "parked — submitted Enter"},
		{"parked dry-run", sessionUnstickResult{Running: true, Parked: true}, true, "parked (dry-run, not submitted)"},
		{"parked submit failed", sessionUnstickResult{Running: true, Parked: true}, false, "parked — submit failed"},
		{"no parked input", sessionUnstickResult{Running: true}, false, "no parked input"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := unstickResultStatus(tt.result, tt.dryRun); got != tt.want {
				t.Errorf("unstickResultStatus(%+v, %v) = %q, want %q", tt.result, tt.dryRun, got, tt.want)
			}
		})
	}
}

func TestWriteSessionUnstickOutput_JSONSummary(t *testing.T) {
	results := []sessionUnstickResult{
		{SessionID: "gc-1", Target: "mayor", Running: true, Parked: true, Submitted: true},
		{SessionID: "gc-2", Target: "witness", Running: true},
		{SessionID: "gc-3", Target: "nux"},
	}
	var stdout, stderr bytes.Buffer
	if code := writeSessionUnstickOutput(results, false, true, true, &stdout, &stderr); code != 0 {
		t.Fatalf("writeSessionUnstickOutput: code = %d, stderr = %s", code, stderr.String())
	}
	var got sessionUnstickJSON
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &got); err != nil {
		t.Fatalf("unmarshal JSON output %q: %v", stdout.String(), err)
	}
	if got.Summary.Scanned != 3 || got.Summary.Parked != 1 || got.Summary.Submitted != 1 {
		t.Errorf("summary = %+v, want scanned=3 parked=1 submitted=1", got.Summary)
	}
	if got.DryRun {
		t.Error("DryRun = true, want false")
	}
	if got.SchemaVersion != "1" {
		t.Errorf("SchemaVersion = %q, want 1", got.SchemaVersion)
	}
}
