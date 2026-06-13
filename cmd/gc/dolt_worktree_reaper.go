package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// worktreeDoltReapResult summarizes one city's worktree-dolt reap pass.
//
// Targets are the worktree dolt sql-server processes identified under the
// city's transient worktree tree; Reaped counts those confirmed terminated
// (exited on SIGTERM or successfully SIGKILLed); Errors records non-fatal
// signal or discovery failures so the supervisor can log them without
// aborting shutdown.
type worktreeDoltReapResult struct {
	Targets []ReapTarget
	Reaped  int
	Errors  []string
}

// worktreeDoltConfigUnderCity reports whether a dolt sql-server --config path
// belongs to a transient worktree under cityPath — i.e. it lives under
// <cityPath>/.gc/worktrees/.
//
// The main managed dolt is deliberately excluded: its config sits under
// <cityPath>/.gc/runtime and its data under <cityPath>/.beads, neither of
// which is in the worktree tree. That separation is what lets shutdown reap
// orphaned worktree dolts without touching the city's own bead store (which
// preserve-mode restart re-adopts and destructive stop already tears down).
// Other cities (e.g. /tmp/city) and the canonical rig clone live outside this
// city's tree and are likewise never matched.
func worktreeDoltConfigUnderCity(configPath, cityPath string) bool {
	if configPath == "" || cityPath == "" {
		return false
	}
	worktreesRoot := filepath.Join(filepath.Clean(cityPath), ".gc", "worktrees")
	clean := filepath.Clean(configPath)
	return clean != worktreesRoot && strings.HasPrefix(clean, worktreesRoot+string(filepath.Separator))
}

// planWorktreeDoltReap selects the worktree dolt sql-server processes to reap
// for a city. A process qualifies when its --config path lives under the
// city's worktree tree (see worktreeDoltConfigUnderCity). Processes without a
// --config flag, the main managed dolt, and dolts belonging to other cities
// are all left untouched.
func planWorktreeDoltReap(procs []DoltProcInfo, cityPath string) []ReapTarget {
	var targets []ReapTarget
	for _, p := range procs {
		cfg := extractConfigPath(p.Argv)
		if !worktreeDoltConfigUnderCity(cfg, cityPath) {
			continue
		}
		targets = append(targets, ReapTarget{
			PID:            p.PID,
			ConfigPath:     cfg,
			RSSBytes:       p.RSSBytes,
			StartTimeTicks: p.StartTimeTicks,
			StartIdentity:  p.StartIdentity,
		})
	}
	return targets
}

// reapCityWorktreeDolts discovers and terminates orphaned worktree dolt
// sql-server processes belonging to cityPath. It sends SIGTERM (letting dolt
// flush and exit cleanly), waits grace, then SIGKILLs any survivor that is
// still the same process — re-discovering first so a PID recycled during the
// grace window is never killed.
//
// discover and kill are injection points for tests; nil falls back to the
// production /proc walker and syscall.Kill. The main managed dolt is never a
// candidate because its config is outside the worktree tree, so this is safe
// to call in both destructive and session-preserving shutdown paths.
func reapCityWorktreeDolts(cityPath string, discover func() ([]DoltProcInfo, error), kill func(pid int, sig syscall.Signal) error, grace time.Duration, stderr io.Writer) worktreeDoltReapResult {
	if discover == nil {
		discover = discoverDoltProcesses
	}
	if kill == nil {
		kill = killProcess
	}

	var result worktreeDoltReapResult
	procs, err := discover()
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("discover dolt processes: %v", err))
		return result
	}
	result.Targets = planWorktreeDoltReap(procs, cityPath)
	if len(result.Targets) == 0 {
		return result
	}

	// Phase 1: SIGTERM every target so dolt can flush and exit on its own.
	termSent := make(map[int]bool, len(result.Targets))
	for _, t := range result.Targets {
		if err := kill(t.PID, syscall.SIGTERM); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				// Vanished between discovery and signal — already gone.
				result.Reaped++
				continue
			}
			result.Errors = append(result.Errors, fmt.Sprintf("pid %d SIGTERM: %v", t.PID, err))
			continue
		}
		termSent[t.PID] = true
	}

	if grace > 0 {
		time.Sleep(grace)
	}

	// Re-discover so SIGKILL only targets processes that are still alive AND
	// still the same process — guarding against PID reuse during the grace
	// window. Without a fresh view we cannot prove identity, so we stop here
	// and let any SIGTERMed process exit on its own.
	refreshed, rerr := discover()
	if rerr != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("re-discover dolt processes: %v", rerr))
		return result
	}
	alive := make(map[int]DoltProcInfo, len(refreshed))
	for _, p := range refreshed {
		alive[p.PID] = p
	}

	// Phase 2: SIGKILL survivors.
	for _, t := range result.Targets {
		if !termSent[t.PID] {
			continue
		}
		proc, stillAlive := alive[t.PID]
		if !stillAlive {
			result.Reaped++ // SIGTERM was enough.
			continue
		}
		if !sameReapProcessIdentity(t, proc) {
			result.Errors = append(result.Errors, fmt.Sprintf("pid %d reused after SIGTERM; skipping SIGKILL", t.PID))
			continue
		}
		if err := kill(t.PID, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				result.Reaped++
				continue
			}
			result.Errors = append(result.Errors, fmt.Sprintf("pid %d SIGKILL: %v", t.PID, err))
			continue
		}
		result.Reaped++
	}

	if stderr != nil {
		fmt.Fprintf(stderr, "gc supervisor: reaped %d/%d worktree dolt sql-server process(es) under %s\n", //nolint:errcheck
			result.Reaped, len(result.Targets), filepath.Join(filepath.Clean(cityPath), ".gc", "worktrees"))
	}
	return result
}

// supervisorReapCityWorktreeDolts is the per-city reap used by the
// reap-worktree-dolts lifecycle hook. It is indirected through a var so tests
// can substitute a fake without walking /proc or signaling real processes.
var supervisorReapCityWorktreeDolts = func(cityPath string, stderr io.Writer) worktreeDoltReapResult {
	return reapCityWorktreeDolts(cityPath, nil, nil, supervisorWorktreeDoltReapGrace, stderr)
}

// reapWorktreeDoltsForCities reaps orphaned worktree dolt sql-server processes
// for every supplied city, returning the aggregate count reaped and the number
// of non-fatal errors. Per-city detail (and any signal failures) is written to
// stderr by the underlying reaper; a one-line aggregate summary is written to
// stdout so the supervisor.log records that the hook ran.
//
// This backs the `gc supervisor reap-worktree-dolts` command wired into the
// generated systemd unit's ExecStopPost. systemd runs ExecStopPost after the
// supervisor exits regardless of how it died, so this covers the crash/SIGKILL
// case that the in-process graceful-shutdown reaper (cmd_supervisor.go toStop
// loop) cannot reach. The main managed dolt is never a candidate — its config
// lives outside the worktree tree — so this is safe in every shutdown path.
func reapWorktreeDoltsForCities(cityPaths []string, stdout, stderr io.Writer) (reaped, failures int) {
	for _, cityPath := range cityPaths {
		res := supervisorReapCityWorktreeDolts(cityPath, stderr)
		reaped += res.Reaped
		failures += len(res.Errors)
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "gc supervisor reap-worktree-dolts: reaped %d worktree dolt sql-server process(es) across %d registered city(ies); %d error(s)\n", //nolint:errcheck
			reaped, len(cityPaths), failures)
	}
	return reaped, failures
}
