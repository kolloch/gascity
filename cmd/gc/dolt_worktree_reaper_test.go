package main

import (
	"io"
	"strings"
	"syscall"
	"testing"
)

func cfgArgv(path string) []string {
	return []string{"dolt", "sql-server", "--config", path}
}

func TestWorktreeDoltConfigUnderCity(t *testing.T) {
	city := "/home/peter/dipcity"
	cases := []struct {
		name    string
		cfg     string
		city    string
		wantHit bool
	}{
		{
			name:    "polecat worktree dolt is under the city worktree tree",
			cfg:     "/home/peter/dipcity/.gc/worktrees/gascity/polecats/nux/.gc/runtime/packs/dolt/dolt-config.yaml",
			city:    city,
			wantHit: true,
		},
		{
			name:    "refinery crew worktree dolt is under the city worktree tree",
			cfg:     "/home/peter/dipcity/.gc/worktrees/dipcity/refinery/.beads/dolt-config.yaml",
			city:    city,
			wantHit: true,
		},
		{
			name:    "main managed dolt under .gc/runtime is NOT a worktree dolt",
			cfg:     "/home/peter/dipcity/.gc/runtime/packs/dolt/dolt-config.yaml",
			city:    city,
			wantHit: false,
		},
		{
			name:    "main managed dolt data under .beads is NOT a worktree dolt",
			cfg:     "/home/peter/dipcity/.beads/dolt/dolt-config.yaml",
			city:    city,
			wantHit: false,
		},
		{
			name:    "a different city's worktree dolt is not ours",
			cfg:     "/tmp/city/.gc/worktrees/gascity/polecats/nux/.gc/runtime/packs/dolt/dolt-config.yaml",
			city:    city,
			wantHit: false,
		},
		{
			name:    "canonical rig clone outside the city tree is protected",
			cfg:     "/home/peter/github.com/kolloch/gascity/.beads/dolt/dolt-config.yaml",
			city:    city,
			wantHit: false,
		},
		{
			name:    "sibling city sharing a name prefix is not matched",
			cfg:     "/home/peter/dipcity-test/.gc/worktrees/gascity/x/dolt-config.yaml",
			city:    city,
			wantHit: false,
		},
		{
			name:    "the worktrees root itself is not a config path",
			cfg:     "/home/peter/dipcity/.gc/worktrees",
			city:    city,
			wantHit: false,
		},
		{
			name:    "empty config path",
			cfg:     "",
			city:    city,
			wantHit: false,
		},
		{
			name:    "empty city path",
			cfg:     "/home/peter/dipcity/.gc/worktrees/gascity/x/dolt-config.yaml",
			city:    "",
			wantHit: false,
		},
		{
			name:    "path traversal escaping the worktree tree is rejected",
			cfg:     "/home/peter/dipcity/.gc/worktrees/../runtime/packs/dolt/dolt-config.yaml",
			city:    city,
			wantHit: false,
		},
		{
			name:    "trailing slash on city path is tolerated",
			cfg:     "/home/peter/dipcity/.gc/worktrees/gascity/x/dolt-config.yaml",
			city:    "/home/peter/dipcity/",
			wantHit: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := worktreeDoltConfigUnderCity(tc.cfg, tc.city)
			if got != tc.wantHit {
				t.Fatalf("worktreeDoltConfigUnderCity(%q, %q) = %v, want %v", tc.cfg, tc.city, got, tc.wantHit)
			}
		})
	}
}

func TestPlanWorktreeDoltReap_SelectsOnlyWorktreeDolts(t *testing.T) {
	city := "/home/peter/dipcity"
	procs := []DoltProcInfo{
		{PID: 100, Argv: cfgArgv("/home/peter/dipcity/.gc/runtime/packs/dolt/dolt-config.yaml")},                  // main — protect
		{PID: 200, Argv: cfgArgv("/home/peter/dipcity/.gc/worktrees/gascity/polecats/nux/.gc/runtime/dolt.yaml")}, // worktree — reap
		{PID: 300, Argv: cfgArgv("/tmp/city/.gc/runtime/packs/dolt/dolt-config.yaml")},                            // other city — protect
		{PID: 400, Argv: cfgArgv("/home/peter/dipcity/.gc/worktrees/dipcity/refinery/.beads/dolt-config.yaml")},   // worktree — reap
		{PID: 500, Argv: []string{"dolt", "sql-server"}},                                                          // no --config — protect
	}
	targets := planWorktreeDoltReap(procs, city)
	gotPIDs := map[int]bool{}
	for _, tg := range targets {
		gotPIDs[tg.PID] = true
	}
	if len(targets) != 2 || !gotPIDs[200] || !gotPIDs[400] {
		t.Fatalf("expected to reap only worktree dolts 200 and 400, got %+v", targets)
	}
	for _, tg := range targets {
		if tg.ConfigPath == "" {
			t.Fatalf("reap target %d missing ConfigPath", tg.PID)
		}
	}
}

// killCall records a single signal delivery for assertion.
type killCall struct {
	pid int
	sig syscall.Signal
}

// stagedDiscover returns a discover func that yields successive process
// snapshots on each call, repeating the last snapshot once exhausted.
func stagedDiscover(snapshots ...[]DoltProcInfo) func() ([]DoltProcInfo, error) {
	calls := 0
	return func() ([]DoltProcInfo, error) {
		idx := calls
		if idx >= len(snapshots) {
			idx = len(snapshots) - 1
		}
		calls++
		return snapshots[idx], nil
	}
}

func TestReapCityWorktreeDolts_SigtermSuffices(t *testing.T) {
	city := "/home/peter/dipcity"
	worktree := DoltProcInfo{PID: 200, Argv: cfgArgv("/home/peter/dipcity/.gc/worktrees/gascity/x/dolt.yaml"), StartTimeTicks: 111}
	main := DoltProcInfo{PID: 100, Argv: cfgArgv("/home/peter/dipcity/.gc/runtime/packs/dolt/dolt-config.yaml"), StartTimeTicks: 222}

	// First snapshot: both alive. After SIGTERM: worktree dolt has exited.
	discover := stagedDiscover(
		[]DoltProcInfo{main, worktree},
		[]DoltProcInfo{main},
	)
	var calls []killCall
	kill := func(pid int, sig syscall.Signal) error {
		calls = append(calls, killCall{pid, sig})
		return nil
	}

	res := reapCityWorktreeDolts(city, discover, kill, 0, io.Discard)

	if len(res.Targets) != 1 || res.Targets[0].PID != 200 {
		t.Fatalf("expected single target PID 200, got %+v", res.Targets)
	}
	if res.Reaped != 1 {
		t.Fatalf("expected Reaped=1, got %d (errors: %v)", res.Reaped, res.Errors)
	}
	if len(calls) != 1 || calls[0] != (killCall{200, syscall.SIGTERM}) {
		t.Fatalf("expected exactly one SIGTERM to PID 200, got %+v", calls)
	}
	for _, c := range calls {
		if c.pid == 100 {
			t.Fatalf("main managed dolt PID 100 must never be signaled, got %+v", calls)
		}
	}
}

func TestReapCityWorktreeDolts_SigkillSurvivor(t *testing.T) {
	city := "/home/peter/dipcity"
	worktree := DoltProcInfo{PID: 200, Argv: cfgArgv("/home/peter/dipcity/.gc/worktrees/gascity/x/dolt.yaml"), StartTimeTicks: 111}

	// Worktree dolt ignores SIGTERM — present in both snapshots, same identity.
	discover := stagedDiscover(
		[]DoltProcInfo{worktree},
		[]DoltProcInfo{worktree},
	)
	var calls []killCall
	kill := func(pid int, sig syscall.Signal) error {
		calls = append(calls, killCall{pid, sig})
		return nil
	}

	res := reapCityWorktreeDolts(city, discover, kill, 0, io.Discard)

	if res.Reaped != 1 {
		t.Fatalf("expected Reaped=1 after SIGKILL, got %d (errors: %v)", res.Reaped, res.Errors)
	}
	want := []killCall{{200, syscall.SIGTERM}, {200, syscall.SIGKILL}}
	if len(calls) != 2 || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("expected SIGTERM then SIGKILL to PID 200, got %+v", calls)
	}
}

func TestReapCityWorktreeDolts_PidReuseGuardSkipsSigkill(t *testing.T) {
	city := "/home/peter/dipcity"
	worktree := DoltProcInfo{PID: 200, Argv: cfgArgv("/home/peter/dipcity/.gc/worktrees/gascity/x/dolt.yaml"), StartTimeTicks: 111}
	// After SIGTERM the original exited and PID 200 was recycled by an
	// unrelated dolt with a different start time — must NOT be SIGKILLed.
	reused := DoltProcInfo{PID: 200, Argv: cfgArgv("/home/peter/dipcity/.gc/worktrees/gascity/x/dolt.yaml"), StartTimeTicks: 999}

	discover := stagedDiscover(
		[]DoltProcInfo{worktree},
		[]DoltProcInfo{reused},
	)
	var calls []killCall
	kill := func(pid int, sig syscall.Signal) error {
		calls = append(calls, killCall{pid, sig})
		return nil
	}

	res := reapCityWorktreeDolts(city, discover, kill, 0, io.Discard)

	for _, c := range calls {
		if c.sig == syscall.SIGKILL {
			t.Fatalf("PID-reuse guard must prevent SIGKILL, got %+v", calls)
		}
	}
	if len(res.Errors) == 0 {
		t.Fatalf("expected a recorded error noting the PID-reuse skip")
	}
}

func TestReapCityWorktreeDolts_NoWorktreeDoltsNoSignals(t *testing.T) {
	city := "/home/peter/dipcity"
	main := DoltProcInfo{PID: 100, Argv: cfgArgv("/home/peter/dipcity/.gc/runtime/packs/dolt/dolt-config.yaml")}
	discover := stagedDiscover([]DoltProcInfo{main})
	var calls []killCall
	kill := func(pid int, sig syscall.Signal) error {
		calls = append(calls, killCall{pid, sig})
		return nil
	}

	res := reapCityWorktreeDolts(city, discover, kill, 0, io.Discard)

	if len(res.Targets) != 0 || res.Reaped != 0 {
		t.Fatalf("expected no targets and no reaps, got targets=%+v reaped=%d", res.Targets, res.Reaped)
	}
	if len(calls) != 0 {
		t.Fatalf("expected no signals when there are no worktree dolts, got %+v", calls)
	}
}

func TestReapCityWorktreeDolts_DiscoverErrorIsRecorded(t *testing.T) {
	city := "/home/peter/dipcity"
	discover := func() ([]DoltProcInfo, error) {
		return nil, io.ErrUnexpectedEOF
	}
	var calls []killCall
	kill := func(pid int, sig syscall.Signal) error {
		calls = append(calls, killCall{pid, sig})
		return nil
	}

	res := reapCityWorktreeDolts(city, discover, kill, 0, io.Discard)

	if len(res.Errors) == 0 {
		t.Fatalf("expected discover error to be recorded")
	}
	if len(calls) != 0 {
		t.Fatalf("must not signal anything when discovery fails, got %+v", calls)
	}
}

func TestReapCityWorktreeDolts_AlreadyGoneOnSigtermCountsReaped(t *testing.T) {
	city := "/home/peter/dipcity"
	worktree := DoltProcInfo{PID: 200, Argv: cfgArgv("/home/peter/dipcity/.gc/worktrees/gascity/x/dolt.yaml"), StartTimeTicks: 111}
	discover := stagedDiscover([]DoltProcInfo{worktree})
	kill := func(_ int, _ syscall.Signal) error {
		return syscall.ESRCH // vanished between discovery and signal
	}

	res := reapCityWorktreeDolts(city, discover, kill, 0, io.Discard)

	if res.Reaped != 1 {
		t.Fatalf("a process that vanished on SIGTERM should count as reaped, got %d (errors %v)", res.Reaped, res.Errors)
	}
}

func TestReapCityDoltsForCities_IteratesEveryCityAndAggregates(t *testing.T) {
	var seen []string
	orig := supervisorReapCityDolts
	t.Cleanup(func() { supervisorReapCityDolts = orig })
	supervisorReapCityDolts = func(cityPath string, _ io.Writer) doltReapResult {
		seen = append(seen, cityPath)
		switch cityPath {
		case "/home/peter/dipcity":
			return doltReapResult{Reaped: 2}
		case "/tmp/city":
			return doltReapResult{Reaped: 1, Errors: []string{"pid 7 SIGKILL: boom"}}
		default:
			return doltReapResult{}
		}
	}

	var stdout, stderr strings.Builder
	reaped, failures := reapCityDoltsForCities([]string{"/home/peter/dipcity", "/tmp/city"}, &stdout, &stderr)

	if reaped != 3 {
		t.Fatalf("aggregate reaped = %d, want 3", reaped)
	}
	if failures != 1 {
		t.Fatalf("aggregate failures = %d, want 1", failures)
	}
	if len(seen) != 2 || seen[0] != "/home/peter/dipcity" || seen[1] != "/tmp/city" {
		t.Fatalf("reaped cities = %v, want [/home/peter/dipcity /tmp/city]", seen)
	}
	if !strings.Contains(stdout.String(), "reaped 3 dolt") {
		t.Fatalf("stdout missing aggregate summary, got %q", stdout.String())
	}
}

func TestReapCityDoltsForCities_EmptyListNeverCallsReaper(t *testing.T) {
	orig := supervisorReapCityDolts
	t.Cleanup(func() { supervisorReapCityDolts = orig })
	called := false
	supervisorReapCityDolts = func(string, io.Writer) doltReapResult {
		called = true
		return doltReapResult{}
	}

	var stdout, stderr strings.Builder
	reaped, failures := reapCityDoltsForCities(nil, &stdout, &stderr)

	if called {
		t.Fatal("the per-city reaper must not be invoked for an empty city list")
	}
	if reaped != 0 || failures != 0 {
		t.Fatalf("reaped=%d failures=%d, want 0/0", reaped, failures)
	}
}

func TestMainDoltConfigPathForCity(t *testing.T) {
	cases := []struct {
		name string
		city string
		want string
	}{
		{
			name: "canonical city path",
			city: "/home/peter/dipcity",
			want: "/home/peter/dipcity/.gc/runtime/packs/dolt/dolt-config.yaml",
		},
		{
			name: "trailing slash on city path is tolerated",
			city: "/home/peter/dipcity/",
			want: "/home/peter/dipcity/.gc/runtime/packs/dolt/dolt-config.yaml",
		},
		{
			name: "empty city path yields empty",
			city: "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mainDoltConfigPathForCity(tc.city); got != tc.want {
				t.Fatalf("mainDoltConfigPathForCity(%q) = %q, want %q", tc.city, got, tc.want)
			}
		})
	}
}

func TestPlanCityDoltReap_SelectsWorktreeAndMainDolts(t *testing.T) {
	city := "/home/peter/dipcity"
	procs := []DoltProcInfo{
		{PID: 100, Argv: cfgArgv("/home/peter/dipcity/.gc/runtime/packs/dolt/dolt-config.yaml")},                  // main — reap (clean slate)
		{PID: 200, Argv: cfgArgv("/home/peter/dipcity/.gc/worktrees/gascity/polecats/nux/.gc/runtime/dolt.yaml")}, // worktree — reap
		{PID: 300, Argv: cfgArgv("/tmp/city/.gc/runtime/packs/dolt/dolt-config.yaml")},                            // other city main — protect
		{PID: 400, Argv: cfgArgv("/home/peter/dipcity/.gc/worktrees/dipcity/refinery/.beads/dolt-config.yaml")},   // worktree — reap
		{PID: 500, Argv: []string{"dolt", "sql-server"}},                                                          // no --config — protect
		{PID: 600, Argv: cfgArgv("/tmp/city/.gc/worktrees/gascity/x/dolt.yaml")},                                  // other city worktree — protect
	}
	targets := planCityDoltReap(procs, city)
	gotPIDs := map[int]bool{}
	for _, tg := range targets {
		gotPIDs[tg.PID] = true
	}
	if len(targets) != 3 || !gotPIDs[100] || !gotPIDs[200] || !gotPIDs[400] {
		t.Fatalf("expected to reap main+worktree dolts 100,200,400, got %+v", targets)
	}
	if gotPIDs[300] || gotPIDs[500] || gotPIDs[600] {
		t.Fatalf("must not reap other-city or unidentified dolts, got %+v", targets)
	}
	for _, tg := range targets {
		if tg.ConfigPath == "" {
			t.Fatalf("reap target %d missing ConfigPath", tg.PID)
		}
	}
}

func TestReapCityDolts_ReapsMainAndWorktreeDolts(t *testing.T) {
	city := "/home/peter/dipcity"
	main := DoltProcInfo{PID: 100, Argv: cfgArgv("/home/peter/dipcity/.gc/runtime/packs/dolt/dolt-config.yaml"), StartTimeTicks: 222}
	worktree := DoltProcInfo{PID: 200, Argv: cfgArgv("/home/peter/dipcity/.gc/worktrees/gascity/x/dolt.yaml"), StartTimeTicks: 111}
	otherCity := DoltProcInfo{PID: 300, Argv: cfgArgv("/tmp/city/.gc/runtime/packs/dolt/dolt-config.yaml"), StartTimeTicks: 333}

	// SIGTERM is enough for both of our city's dolts; the other city's main
	// dolt is never a candidate and stays alive across both snapshots.
	discover := stagedDiscover(
		[]DoltProcInfo{main, worktree, otherCity},
		[]DoltProcInfo{otherCity},
	)
	var calls []killCall
	kill := func(pid int, sig syscall.Signal) error {
		calls = append(calls, killCall{pid, sig})
		return nil
	}

	res := reapCityDolts(city, discover, kill, 0, io.Discard)

	if res.Reaped != 2 {
		t.Fatalf("expected Reaped=2 (main+worktree), got %d (errors: %v)", res.Reaped, res.Errors)
	}
	signaled := map[int]bool{}
	for _, c := range calls {
		signaled[c.pid] = true
		if c.pid == 300 {
			t.Fatalf("another city's main dolt PID 300 must never be signaled, got %+v", calls)
		}
	}
	if !signaled[100] || !signaled[200] {
		t.Fatalf("expected SIGTERM to both main (100) and worktree (200) dolts, got %+v", calls)
	}
}

func TestReapCityDolts_MainDoltSurvivingSigtermGetsSigkill(t *testing.T) {
	city := "/home/peter/dipcity"
	main := DoltProcInfo{PID: 100, Argv: cfgArgv("/home/peter/dipcity/.gc/runtime/packs/dolt/dolt-config.yaml"), StartTimeTicks: 222}

	// Main dolt ignores SIGTERM — present in both snapshots with the same
	// identity, so the clean-slate reap escalates to SIGKILL.
	discover := stagedDiscover(
		[]DoltProcInfo{main},
		[]DoltProcInfo{main},
	)
	var calls []killCall
	kill := func(pid int, sig syscall.Signal) error {
		calls = append(calls, killCall{pid, sig})
		return nil
	}

	res := reapCityDolts(city, discover, kill, 0, io.Discard)

	if res.Reaped != 1 {
		t.Fatalf("expected Reaped=1 after SIGKILL of main dolt, got %d (errors: %v)", res.Reaped, res.Errors)
	}
	want := []killCall{{100, syscall.SIGTERM}, {100, syscall.SIGKILL}}
	if len(calls) != 2 || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("expected SIGTERM then SIGKILL to main dolt PID 100, got %+v", calls)
	}
}
