package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveManagedDoltLogCapDefaults(t *testing.T) {
	t.Setenv("GC_DOLT_LOG_MAX_BYTES", "")
	t.Setenv("GC_DOLT_LOG_KEEP_TAIL_BYTES", "")
	maxBytes, keepTail := resolveManagedDoltLogCap()
	if maxBytes != defaultManagedDoltLogMaxBytes {
		t.Errorf("maxBytes = %d, want default %d", maxBytes, defaultManagedDoltLogMaxBytes)
	}
	if keepTail != defaultManagedDoltLogKeepTailBytes {
		t.Errorf("keepTail = %d, want default %d", keepTail, defaultManagedDoltLogKeepTailBytes)
	}
}

func TestResolveManagedDoltLogCapEnvOverrides(t *testing.T) {
	t.Setenv("GC_DOLT_LOG_MAX_BYTES", "5000")
	t.Setenv("GC_DOLT_LOG_KEEP_TAIL_BYTES", "1000")
	maxBytes, keepTail := resolveManagedDoltLogCap()
	if maxBytes != 5000 {
		t.Errorf("maxBytes = %d, want 5000", maxBytes)
	}
	if keepTail != 1000 {
		t.Errorf("keepTail = %d, want 1000", keepTail)
	}
}

func TestResolveManagedDoltLogCapDisabledWhenNonPositive(t *testing.T) {
	for _, raw := range []string{"0", "-1"} {
		t.Setenv("GC_DOLT_LOG_MAX_BYTES", raw)
		t.Setenv("GC_DOLT_LOG_KEEP_TAIL_BYTES", "1000")
		maxBytes, keepTail := resolveManagedDoltLogCap()
		if maxBytes != 0 || keepTail != 0 {
			t.Errorf("GC_DOLT_LOG_MAX_BYTES=%s: got (%d,%d), want (0,0)", raw, maxBytes, keepTail)
		}
	}
}

func TestResolveManagedDoltLogCapClampsKeepTail(t *testing.T) {
	// keepTail >= maxBytes is clamped to maxBytes/2 so the retained tail never
	// immediately re-trips the cap.
	t.Setenv("GC_DOLT_LOG_MAX_BYTES", "1000")
	t.Setenv("GC_DOLT_LOG_KEEP_TAIL_BYTES", "4000")
	maxBytes, keepTail := resolveManagedDoltLogCap()
	if maxBytes != 1000 {
		t.Fatalf("maxBytes = %d, want 1000", maxBytes)
	}
	if keepTail != 500 {
		t.Errorf("keepTail = %d, want clamped 500", keepTail)
	}

	// Negative keepTail floors at 0.
	t.Setenv("GC_DOLT_LOG_KEEP_TAIL_BYTES", "-7")
	if _, keepTail := resolveManagedDoltLogCap(); keepTail != 0 {
		t.Errorf("negative keepTail = %d, want 0", keepTail)
	}
}

func TestResolveManagedDoltLogCapInvalidFallsBackToDefault(t *testing.T) {
	t.Setenv("GC_DOLT_LOG_MAX_BYTES", "not-a-number")
	t.Setenv("GC_DOLT_LOG_KEEP_TAIL_BYTES", "also-bad")
	maxBytes, keepTail := resolveManagedDoltLogCap()
	if maxBytes != defaultManagedDoltLogMaxBytes {
		t.Errorf("maxBytes = %d, want default %d", maxBytes, defaultManagedDoltLogMaxBytes)
	}
	if keepTail != defaultManagedDoltLogKeepTailBytes {
		t.Errorf("keepTail = %d, want default %d", keepTail, defaultManagedDoltLogKeepTailBytes)
	}
}

func TestCapManagedDoltLogNoOpUnderCap(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dolt.log")
	content := []byte("small log content")
	if err := os.WriteFile(logPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	capped, err := capManagedDoltLog(logPath, 1000, 200)
	if err != nil {
		t.Fatalf("capManagedDoltLog: %v", err)
	}
	if capped {
		t.Error("capped = true, want false for under-cap log")
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Error("under-cap log content was modified")
	}
	if _, err := os.Stat(logPath + ".1"); !os.IsNotExist(err) {
		t.Error("rotated .1 should not exist for under-cap log")
	}
}

func TestCapManagedDoltLogMissingFileIsNoError(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "absent.log")
	capped, err := capManagedDoltLog(logPath, 1000, 200)
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if capped {
		t.Error("capped = true for missing file")
	}
}

func TestCapManagedDoltLogDisabledWhenMaxNonPositive(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dolt.log")
	big := bytes.Repeat([]byte("x"), 5000)
	if err := os.WriteFile(logPath, big, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, maxBytes := range []int64{0, -1} {
		capped, err := capManagedDoltLog(logPath, maxBytes, 200)
		if err != nil {
			t.Fatalf("maxBytes=%d: %v", maxBytes, err)
		}
		if capped {
			t.Errorf("maxBytes=%d: capped = true, want false (disabled)", maxBytes)
		}
		info, err := os.Stat(logPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != int64(len(big)) {
			t.Errorf("maxBytes=%d: log size = %d, want unchanged %d", maxBytes, info.Size(), len(big))
		}
	}
}

func TestCapManagedDoltLogRotatesOversized(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dolt.log")
	head := strings.Repeat("H", 4800)
	tail := strings.Repeat("T", 200)
	if err := os.WriteFile(logPath, []byte(head+tail), 0o644); err != nil {
		t.Fatal(err)
	}
	capped, err := capManagedDoltLog(logPath, 1000, 200)
	if err != nil {
		t.Fatalf("capManagedDoltLog: %v", err)
	}
	if !capped {
		t.Fatal("capped = false, want true for oversized log")
	}
	// Live log truncated to zero.
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("live log size = %d, want 0 after rotation", info.Size())
	}
	// Rotated generation holds the most recent keepTail bytes.
	rotated, err := os.ReadFile(logPath + ".1")
	if err != nil {
		t.Fatalf("read rotated .1: %v", err)
	}
	if string(rotated) != tail {
		t.Errorf("rotated .1 = %q (len %d), want the last 200 bytes", truncateForMsg(string(rotated)), len(rotated))
	}
}

func TestCapManagedDoltLogKeepTailZeroDropsGeneration(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dolt.log")
	if err := os.WriteFile(logPath, bytes.Repeat([]byte("x"), 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	// A pre-existing rotated generation must be removed when keepTail is 0 so a
	// stale .1 never implies it mirrors current content.
	if err := os.WriteFile(logPath+".1", []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	capped, err := capManagedDoltLog(logPath, 1000, 0)
	if err != nil {
		t.Fatalf("capManagedDoltLog: %v", err)
	}
	if !capped {
		t.Fatal("capped = false, want true")
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("live log size = %d, want 0", info.Size())
	}
	if _, err := os.Stat(logPath + ".1"); !os.IsNotExist(err) {
		t.Error("stale .1 should be removed when keepTail is 0")
	}
}

// TestCapManagedDoltLogCopytruncateAllowsLiveAppendWithoutHole is the core
// correctness proof for preserve mode: a live writer holding the log open in
// O_APPEND (as the managed dolt sql-server does — its inherited stdout/stderr
// fd) must keep writing at offset 0 after we truncate in place, never leaving
// a sparse hole the size of the pre-cap log. A rename-based rotation would
// fail this test because the live writer would keep appending to the renamed
// inode instead of the capped path.
func TestCapManagedDoltLogCopytruncateAllowsLiveAppendWithoutHole(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dolt.log")

	live, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close() //nolint:errcheck

	if _, err := live.Write(bytes.Repeat([]byte("x"), 5000)); err != nil {
		t.Fatal(err)
	}

	capped, err := capManagedDoltLog(logPath, 1000, 200)
	if err != nil {
		t.Fatalf("capManagedDoltLog: %v", err)
	}
	if !capped {
		t.Fatal("capped = false, want true")
	}

	after := []byte("AFTER-CAP")
	if _, err := live.Write(after); err != nil {
		t.Fatalf("append after cap: %v", err)
	}

	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(after)) {
		t.Fatalf("post-cap log size = %d, want %d (a larger size means a sparse hole — append did not resolve to offset 0)", info.Size(), len(after))
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, after) {
		t.Errorf("post-cap log content = %q, want %q", truncateForMsg(string(got)), after)
	}
}

func TestManagedDoltLogPathForCity(t *testing.T) {
	t.Setenv("GC_CITY_RUNTIME_DIR", "")
	city := t.TempDir()
	got := managedDoltLogPathForCity(city)
	want := filepath.Join(normalizePathForCompare(city), ".gc", "runtime", "packs", "dolt", "dolt.log")
	if normalizePathForCompare(got) != normalizePathForCompare(want) {
		t.Errorf("managedDoltLogPathForCity(%q) = %q, want %q", city, got, want)
	}
}

func TestCapManagedDoltLogsForCitiesCapsEachCity(t *testing.T) {
	t.Setenv("GC_CITY_RUNTIME_DIR", "")
	city1 := t.TempDir()
	city2 := t.TempDir()
	logs := make([]string, 0, 2)
	for _, c := range []string{city1, city2} {
		logPath := managedDoltLogPathForCity(c)
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(logPath, bytes.Repeat([]byte("x"), 5000), 0o644); err != nil {
			t.Fatal(err)
		}
		logs = append(logs, logPath)
	}

	var out, errBuf bytes.Buffer
	capManagedDoltLogsForCities([]string{city1, city2}, 1000, 200, &out, &errBuf)

	if errBuf.Len() != 0 {
		t.Errorf("unexpected stderr: %s", errBuf.String())
	}
	for _, logPath := range logs {
		info, err := os.Stat(logPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != 0 {
			t.Errorf("log %s not capped: size = %d", logPath, info.Size())
		}
	}
}

func TestCapManagedDoltLogsForCitiesSkipsBlankPaths(t *testing.T) {
	// Must not panic or error on empty/blank city paths.
	var out, errBuf bytes.Buffer
	capManagedDoltLogsForCities([]string{"", "   "}, 1000, 200, &out, &errBuf)
	if errBuf.Len() != 0 {
		t.Errorf("unexpected stderr for blank paths: %s", errBuf.String())
	}
}

func TestCapRunningCityDoltLogsOnlyCapsStartedCities(t *testing.T) {
	t.Setenv("GC_CITY_RUNTIME_DIR", "")
	t.Setenv("GC_DOLT_LOG_MAX_BYTES", "1000")
	t.Setenv("GC_DOLT_LOG_KEEP_TAIL_BYTES", "200")

	startedCity := t.TempDir()
	pendingCity := t.TempDir()
	writeOversizedLog := func(city string) string {
		logPath := managedDoltLogPathForCity(city)
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(logPath, bytes.Repeat([]byte("x"), 5000), 0o644); err != nil {
			t.Fatal(err)
		}
		return logPath
	}
	startedLog := writeOversizedLog(startedCity)
	pendingLog := writeOversizedLog(pendingCity)

	reg := newCityRegistry()
	reg.Add(startedCity, &managedCity{name: "started", started: true})
	reg.Add(pendingCity, &managedCity{name: "pending", started: false})

	var out, errBuf bytes.Buffer
	capRunningCityDoltLogs(reg, &out, &errBuf)

	if errBuf.Len() != 0 {
		t.Errorf("unexpected stderr: %s", errBuf.String())
	}
	startedInfo, err := os.Stat(startedLog)
	if err != nil {
		t.Fatal(err)
	}
	if startedInfo.Size() != 0 {
		t.Errorf("started city log not capped: size = %d", startedInfo.Size())
	}
	pendingInfo, err := os.Stat(pendingLog)
	if err != nil {
		t.Fatal(err)
	}
	if pendingInfo.Size() != 5000 {
		t.Errorf("pending (not started) city log size = %d, want unchanged 5000", pendingInfo.Size())
	}
}

func TestCapRunningCityDoltLogsNilRegistryIsNoOp(_ *testing.T) {
	// Defensive: must not panic with a nil registry.
	var out, errBuf bytes.Buffer
	capRunningCityDoltLogs(nil, &out, &errBuf)
}

func truncateForMsg(s string) string {
	const limit = 64
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "...(truncated)"
}
