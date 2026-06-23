package runtime

import (
	"os"
	"strings"
	"time"
)

// FormatBeacon returns a startup identification string that appears in
// the agent's initial prompt. When an agent crashes and restarts in a
// new session, this beacon makes the predecessor session discoverable
// in tools like Claude Code's /resume picker.
//
// Format: [city-name] agent-name • timestamp
//
// The timestamp is rendered in the operator's local timezone — honoring
// the $TZ environment variable, falling back to the process-local zone
// (time.Local) — and carries an explicit UTC offset (RFC 3339). This
// keeps the human-facing beacon in wall-clock time rather than an
// ambiguous, zone-less string that reads as UTC.
//
// If includePrimeInstruction is true, the beacon also tells the agent
// to run "gc prime" manually. This is needed for non-hook agents that
// won't auto-run gc prime on session restart.
func FormatBeacon(cityName, agentName string, includePrimeInstruction bool) string {
	return FormatBeaconAt(cityName, agentName, includePrimeInstruction, time.Now())
}

// FormatBeaconAt is like FormatBeacon but accepts an explicit time
// for testability.
func FormatBeaconAt(cityName, agentName string, includePrimeInstruction bool, t time.Time) string {
	stamp := t.In(beaconLocation()).Format("2006-01-02T15:04:05Z07:00")
	beacon := "[" + cityName + "] " + agentName + " • " + stamp
	if includePrimeInstruction {
		beacon += "\n\nRun `gc prime` to initialize your context."
	}
	return beacon
}

// beaconLocation resolves the timezone used to render beacon timestamps.
// It honors the $TZ environment variable when it names a loadable zone,
// otherwise falls back to the process-local timezone (time.Local). An
// empty or unparseable $TZ falls back rather than erroring, so the beacon
// is always emitted.
func beaconLocation() *time.Location {
	if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.Local
}
