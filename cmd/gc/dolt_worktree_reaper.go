package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// doltReapResult summarizes one city's dolt-reap pass.
//
// Targets are the dolt sql-server processes selected for the city; Reaped
// counts those confirmed terminated (exited on SIGTERM or successfully
// SIGKILLed); Errors records non-fatal signal or discovery failures so the
// supervisor can log them without aborting shutdown.
type doltReapResult struct {
	Targets []ReapTarget
	Reaped  int
	Errors  []string
}

// worktreeDoltConfigUnderCity reports whether a dolt sql-server --config path
// belongs to a transient worktree under cityPath — i.e. it lives under
// <cityPath>/.gc/worktrees/.
//
// The main managed dolt is deliberately excluded here: its config sits under
// <cityPath>/.gc/runtime and its data under <cityPath>/.beads, neither of
// which is in the worktree tree. That separation is what lets the in-process
// graceful shutdown reap orphaned worktree dolts without touching the city's
// own bead store (which preserve-mode restart re-adopts). The clean-slate
// post-shutdown hook reaps the main dolt separately via mainDoltConfigForCity.
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

// mainDoltConfigPathForCity returns the canonical --config path of a city's
// main managed dolt sql-server: <cityPath>/.gc/runtime/packs/dolt/dolt-config.yaml.
// This is the path the managed-dolt launcher passes to `dolt sql-server`
// (resolveManagedDoltRuntimeLayout's default branch via citylayout.PackStateDir),
// so a process whose --config equals it is that city's own bead-store dolt.
//
// Unlike worktree dolts, the main dolt holds the city's bead store, so it is
// reaped only by the post-shutdown clean-slate hook (reapCityDolts), never by
// the in-process worktree reap (reapCityWorktreeDolts) that runs while sessions
// may be preserved for re-adoption.
func mainDoltConfigPathForCity(cityPath string) string {
	if strings.TrimSpace(cityPath) == "" {
		return ""
	}
	return filepath.Join(citylayout.PackStateDir(cityPath, "dolt"), "dolt-config.yaml")
}

// mainDoltConfigForCity reports whether a dolt sql-server --config path is the
// main managed dolt for cityPath. The comparison is lexical (filepath.Clean on
// both sides), matching worktreeDoltConfigUnderCity, so the planners stay pure
// and filesystem-independent for tests.
func mainDoltConfigForCity(configPath, cityPath string) bool {
	mainCfg := mainDoltConfigPathForCity(cityPath)
	if configPath == "" || mainCfg == "" {
		return false
	}
	return filepath.Clean(configPath) == filepath.Clean(mainCfg)
}

// reapTargetFromProc projects a discovered process into a ReapTarget carrying
// the identity fields the PID-reuse guard needs.
func reapTargetFromProc(p DoltProcInfo, cfg string) ReapTarget {
	return ReapTarget{
		PID:            p.PID,
		ConfigPath:     cfg,
		RSSBytes:       p.RSSBytes,
		StartTimeTicks: p.StartTimeTicks,
		StartIdentity:  p.StartIdentity,
	}
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
		targets = append(targets, reapTargetFromProc(p, cfg))
	}
	return targets
}

// planCityDoltReap selects every dolt sql-server process that belongs to the
// city: its worktree dolts (under <city>/.gc/worktrees/) PLUS its main managed
// dolt (config == <city>/.gc/runtime/packs/dolt/dolt-config.yaml).
//
// This is the clean-slate selection behind the systemd ExecStopPost hook
// (ga-s5y): when the supervisor stops, the whole city goes down, so its
// bead-store dolt is reaped too. That retires the hand-installed
// dolt-cleanup.conf drop-in and fixes the orphan-main-dolt re-adoption stall
// (pe-t07v), where a surviving main dolt was reused by the next supervisor and
// its dolt.log grew unbounded. Dolts of other cities and processes without a
// --config are never selected.
func planCityDoltReap(procs []DoltProcInfo, cityPath string) []ReapTarget {
	var targets []ReapTarget
	for _, p := range procs {
		cfg := extractConfigPath(p.Argv)
		if !worktreeDoltConfigUnderCity(cfg, cityPath) && !mainDoltConfigForCity(cfg, cityPath) {
			continue
		}
		targets = append(targets, reapTargetFromProc(p, cfg))
	}
	return targets
}

// reapDoltSelection runs the SIGTERM→grace→SIGKILL reap with a PID-reuse guard
// over the dolt sql-server processes selectTargets picks out of a freshly-
// discovered process list. It is the shared engine behind reapCityWorktreeDolts
// (worktree-only, in-process shutdown) and reapCityDolts (main + worktree,
// systemd ExecStopPost).
//
// It sends SIGTERM (letting dolt flush and exit cleanly), waits grace, then
// SIGKILLs any survivor that is still the same process — re-discovering first so
// a PID recycled during the grace window is never killed. discover and kill are
// injection points for tests; nil falls back to the production /proc walker and
// syscall.Kill.
func reapDoltSelection(cityPath string, selectTargets func(procs []DoltProcInfo, cityPath string) []ReapTarget, discover func() ([]DoltProcInfo, error), kill func(pid int, sig syscall.Signal) error, grace time.Duration) doltReapResult {
	if discover == nil {
		discover = discoverDoltProcesses
	}
	if kill == nil {
		kill = killProcess
	}

	var result doltReapResult
	procs, err := discover()
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("discover dolt processes: %v", err))
		return result
	}
	result.Targets = selectTargets(procs, cityPath)
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

	return result
}

// reapCityWorktreeDolts discovers and terminates orphaned worktree dolt
// sql-server processes belonging to cityPath. The main managed dolt is never a
// candidate because its config is outside the worktree tree, so this is safe to
// call in both destructive and session-preserving shutdown paths — including
// the in-process graceful shutdown where preserve mode keeps the main dolt
// alive for fast re-adoption. The systemd ExecStopPost hook uses reapCityDolts
// instead so the bead-store dolt is reaped once the supervisor has stopped.
func reapCityWorktreeDolts(cityPath string, discover func() ([]DoltProcInfo, error), kill func(pid int, sig syscall.Signal) error, grace time.Duration, stderr io.Writer) doltReapResult {
	result := reapDoltSelection(cityPath, planWorktreeDoltReap, discover, kill, grace)
	if stderr != nil && len(result.Targets) > 0 {
		fmt.Fprintf(stderr, "gc supervisor: reaped %d/%d worktree dolt sql-server process(es) under %s\n", //nolint:errcheck
			result.Reaped, len(result.Targets), filepath.Join(filepath.Clean(cityPath), ".gc", "worktrees"))
	}
	return result
}

// reapCityDolts discovers and terminates every dolt sql-server process
// belonging to cityPath — its worktree dolts AND its main managed dolt — using
// the same SIGTERM→grace→SIGKILL reap with a PID-reuse guard.
//
// It backs the `gc supervisor reap-city-dolts` command wired into the generated
// systemd unit's ExecStopPost, which systemd runs after the supervisor exits
// regardless of how it died. Reaping the bead-store dolt is safe there because
// the supervisor has already stopped — nothing is using it. This is the
// clean-slate behavior (ga-s5y, mayor decision) that retires the hand-installed
// dolt-cleanup.conf drop-in and fixes the orphan-main-dolt re-adoption stall
// (pe-t07v). It deliberately does NOT run in the in-process graceful path,
// where preserve mode keeps the main dolt alive for fast re-adoption.
func reapCityDolts(cityPath string, discover func() ([]DoltProcInfo, error), kill func(pid int, sig syscall.Signal) error, grace time.Duration, stderr io.Writer) doltReapResult {
	result := reapDoltSelection(cityPath, planCityDoltReap, discover, kill, grace)
	if stderr != nil && len(result.Targets) > 0 {
		fmt.Fprintf(stderr, "gc supervisor: reaped %d/%d dolt sql-server process(es) for city %s\n", //nolint:errcheck
			result.Reaped, len(result.Targets), filepath.Clean(cityPath))
	}
	return result
}

// supervisorReapCityDolts is the per-city reap used by the reap-city-dolts
// lifecycle hook. It is indirected through a var so tests can substitute a fake
// without walking /proc or signaling real processes.
var supervisorReapCityDolts = func(cityPath string, stderr io.Writer) doltReapResult {
	return reapCityDolts(cityPath, nil, nil, supervisorDoltReapGrace, stderr)
}

// reapCityDoltsForCities reaps dolt sql-server processes (worktree and main
// managed) for every supplied city, returning the aggregate count reaped and
// the number of non-fatal errors. Per-city detail (and any signal failures) is
// written to stderr by the underlying reaper; a one-line aggregate summary is
// written to stdout so the supervisor.log records that the hook ran.
//
// This backs the `gc supervisor reap-city-dolts` command wired into the
// generated systemd unit's ExecStopPost. systemd runs ExecStopPost after the
// supervisor exits regardless of how it died, so this covers the crash/SIGKILL
// case that the in-process graceful-shutdown reaper (cmd_supervisor.go toStop
// loop) cannot reach, and — clean-slate (ga-s5y) — reaps each city's main
// managed dolt too so a surviving bead-store dolt cannot stall the next
// supervisor's re-adoption (pe-t07v).
func reapCityDoltsForCities(cityPaths []string, stdout, stderr io.Writer) (reaped, failures int) {
	for _, cityPath := range cityPaths {
		res := supervisorReapCityDolts(cityPath, stderr)
		reaped += res.Reaped
		failures += len(res.Errors)
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "gc supervisor reap-city-dolts: reaped %d dolt sql-server process(es) across %d registered city(ies); %d error(s)\n", //nolint:errcheck
			reaped, len(cityPaths), failures)
	}
	return reaped, failures
}
