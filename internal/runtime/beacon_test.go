package runtime

import (
	"strings"
	"testing"
	"time"
)

func TestFormatBeaconAt_Basic(t *testing.T) {
	t.Setenv("TZ", "UTC")
	ts := time.Date(2026, 2, 26, 15, 30, 0, 0, time.UTC)
	got := FormatBeaconAt("bright-lights", "mayor", false, ts)
	want := "[bright-lights] mayor • 2026-02-26T15:30:00Z"
	if got != want {
		t.Errorf("FormatBeaconAt() = %q, want %q", got, want)
	}
}

func TestFormatBeaconAt_QualifiedAgent(t *testing.T) {
	t.Setenv("TZ", "UTC")
	ts := time.Date(2026, 2, 26, 15, 30, 0, 0, time.UTC)
	got := FormatBeaconAt("bright-lights", "hello-world/polecat", false, ts)
	want := "[bright-lights] hello-world/polecat • 2026-02-26T15:30:00Z"
	if got != want {
		t.Errorf("FormatBeaconAt() = %q, want %q", got, want)
	}
}

func TestFormatBeaconAt_WithPrimeInstruction(t *testing.T) {
	t.Setenv("TZ", "UTC")
	ts := time.Date(2026, 2, 26, 15, 30, 0, 0, time.UTC)
	got := FormatBeaconAt("bright-lights", "worker", true, ts)
	if !strings.HasPrefix(got, "[bright-lights] worker • 2026-02-26T15:30:00Z") {
		t.Errorf("beacon should start with identification, got %q", got)
	}
	if !strings.Contains(got, "gc prime`") {
		t.Errorf("beacon should include gc prime instruction, got %q", got)
	}
}

func TestFormatBeaconAt_NoPrimeInstruction(t *testing.T) {
	t.Setenv("TZ", "UTC")
	ts := time.Date(2026, 2, 26, 15, 30, 0, 0, time.UTC)
	got := FormatBeaconAt("bright-lights", "mayor", false, ts)
	if strings.Contains(got, "gc prime") {
		t.Errorf("beacon should NOT include gc prime for hook agents, got %q", got)
	}
}

// TestFormatBeaconAt_HonorsTZEnv verifies the beacon renders the timestamp in
// the zone named by $TZ and labels it with an explicit offset, rather than an
// ambiguous, zone-less string. This is the user-facing fix: the operator wants
// to read the beacon in their own wall-clock time (Europe/Zurich), not UTC.
func TestFormatBeaconAt_HonorsTZEnv(t *testing.T) {
	if _, err := time.LoadLocation("Europe/Zurich"); err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	t.Setenv("TZ", "Europe/Zurich")
	// 07:47:19 UTC on a summer day is 09:47:19 CEST (+02:00).
	ts := time.Date(2026, 5, 19, 7, 47, 19, 0, time.UTC)
	got := FormatBeaconAt("gascity", "mayor", false, ts)
	want := "[gascity] mayor • 2026-05-19T09:47:19+02:00"
	if got != want {
		t.Errorf("FormatBeaconAt() = %q, want %q", got, want)
	}
}

// TestFormatBeaconAt_ConvertsUTCInstantToZone verifies a UTC time.Time is
// converted into the configured zone (not merely re-labeled), exercising the
// winter offset where Zurich is +01:00 and New York is -05:00.
func TestFormatBeaconAt_ConvertsUTCInstantToZone(t *testing.T) {
	if _, err := time.LoadLocation("America/New_York"); err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	t.Setenv("TZ", "America/New_York")
	// 15:30:00 UTC on a winter day is 10:30:00 EST (-05:00).
	ts := time.Date(2026, 2, 26, 15, 30, 0, 0, time.UTC)
	got := FormatBeaconAt("bright-lights", "mayor", false, ts)
	want := "[bright-lights] mayor • 2026-02-26T10:30:00-05:00"
	if got != want {
		t.Errorf("FormatBeaconAt() = %q, want %q", got, want)
	}
}

// TestFormatBeaconAt_InvalidTZFallsBack verifies an unparseable $TZ does not
// panic or error; it falls back to the process-local zone and still emits a
// parseable RFC3339 timestamp.
func TestFormatBeaconAt_InvalidTZFallsBack(t *testing.T) {
	t.Setenv("TZ", "Not/ARealZone")
	ts := time.Date(2026, 2, 26, 15, 30, 0, 0, time.UTC)
	got := FormatBeaconAt("bright-lights", "mayor", false, ts)
	if !strings.HasPrefix(got, "[bright-lights] mayor • ") {
		t.Fatalf("beacon should retain identification prefix, got %q", got)
	}
	parts := strings.SplitN(got, " • ", 2)
	if len(parts) != 2 {
		t.Fatalf("expected beacon with bullet separator, got %q", got)
	}
	if _, err := time.Parse(time.RFC3339, parts[1]); err != nil {
		t.Errorf("timestamp %q not parseable as RFC3339: %v", parts[1], err)
	}
}

func TestFormatBeacon_ContainsTimestamp(t *testing.T) {
	got := FormatBeacon("my-city", "worker", false)
	if !strings.HasPrefix(got, "[my-city] worker • ") {
		t.Errorf("FormatBeacon() = %q, want prefix %q", got, "[my-city] worker • ")
	}
	parts := strings.SplitN(got, " • ", 2)
	if len(parts) != 2 {
		t.Fatalf("expected beacon with bullet separator, got %q", got)
	}
	if _, err := time.Parse(time.RFC3339, parts[1]); err != nil {
		t.Errorf("timestamp %q not parseable as RFC3339: %v", parts[1], err)
	}
}
