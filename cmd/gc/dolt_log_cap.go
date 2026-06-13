package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Managed dolt log-cap defaults. In preserve mode
// (GC_SUPERVISOR_PRESERVE_SESSIONS_ON_SIGNAL=1) the main managed dolt
// sql-server is kept alive across supervisor restarts for fast re-adoption, so
// the same process appends warning-level query diagnostics to its log for as
// long as the city runs — days, across many restarts. Left unbounded that log
// reached hundreds of megabytes in production (pe-t07v reported ~100MB of
// repeated warning spam; 375MB has been observed), and re-adoption dragged for
// minutes. Capping the log keeps preserve-mode fast re-adoption viable without
// trading it away for the clean-slate main-dolt kill (ga-1op, the preserve
// alternative to ga-s5y Option 1).
const (
	defaultManagedDoltLogMaxBytes      = 64 << 20 // 64 MiB
	defaultManagedDoltLogKeepTailBytes = 8 << 20  // 8 MiB
)

// resolveManagedDoltLogCap resolves the managed dolt log cap and the size of
// the recent tail retained on rotation from the environment, falling back to
// the package defaults.
//
//   - GC_DOLT_LOG_MAX_BYTES: the size above which the log is rotated. A
//     non-positive value disables capping entirely (returns 0, 0).
//   - GC_DOLT_LOG_KEEP_TAIL_BYTES: bytes of the most recent log preserved to
//     "<log>.1" on rotation. Floored at 0 and clamped below maxBytes so the
//     retained tail never immediately re-trips the cap.
//
// Invalid integers fall back to the defaults rather than failing — log hygiene
// must never block the server.
func resolveManagedDoltLogCap() (maxBytes, keepTail int64) {
	maxBytes = doltLogEnvInt64("GC_DOLT_LOG_MAX_BYTES", defaultManagedDoltLogMaxBytes)
	keepTail = doltLogEnvInt64("GC_DOLT_LOG_KEEP_TAIL_BYTES", defaultManagedDoltLogKeepTailBytes)
	if maxBytes <= 0 {
		return 0, 0
	}
	if keepTail < 0 {
		keepTail = 0
	}
	if keepTail >= maxBytes {
		keepTail = maxBytes / 2
	}
	return maxBytes, keepTail
}

// doltLogEnvInt64 reads an int64 from the named environment variable, returning
// def when it is unset, blank, or not a valid integer.
func doltLogEnvInt64(key string, def int64) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// capManagedDoltLog bounds a managed dolt sql-server log file at maxBytes.
//
// It uses copytruncate semantics — preserve the most recent keepTail bytes to
// "<logPath>.1", then truncate the live file in place — rather than a rename,
// because the managed dolt sql-server holds the log open for its whole
// lifetime (its inherited stdout/stderr fd, see startManagedDoltSQLServer). In
// preserve mode that process is never relaunched, so a rename would only leave
// dolt appending to the renamed inode while the live path never shrank. The
// dolt fd is opened O_APPEND, so after truncation the kernel resolves the next
// append to the new end-of-file (offset 0) with no sparse hole.
//
// Returns whether a rotation occurred. A non-positive maxBytes disables
// capping. A missing log file is reported as no rotation and no error so
// callers can invoke this unconditionally for any city.
func capManagedDoltLog(logPath string, maxBytes, keepTail int64) (bool, error) {
	if strings.TrimSpace(logPath) == "" || maxBytes <= 0 {
		return false, nil
	}
	info, err := os.Stat(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat dolt log %q: %w", logPath, err)
	}
	if info.IsDir() {
		return false, fmt.Errorf("dolt log path %q is a directory", logPath)
	}
	if info.Size() <= maxBytes {
		return false, nil
	}
	if err := rotateManagedDoltLogTail(logPath, info.Size(), keepTail); err != nil {
		return false, err
	}
	return true, nil
}

// rotateManagedDoltLogTail preserves the most recent keepTail bytes of logPath
// (as of size) into "<logPath>.1" and then truncates logPath to zero. When
// keepTail is 0 it drops any pre-existing rotated generation so a stale ".1"
// never implies it mirrors current content.
//
// A small window between sampling size and truncating can drop the few bytes a
// concurrent live writer appends in between; that is the inherent and accepted
// race of copytruncate rotation and only ever loses a sliver of the newest log
// output, never bead or server state.
func rotateManagedDoltLogTail(logPath string, size, keepTail int64) error {
	rotated := logPath + ".1"
	if keepTail <= 0 {
		if err := os.Remove(rotated); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale rotated dolt log %q: %w", rotated, err)
		}
	} else {
		tailStart := size - keepTail
		if tailStart < 0 {
			tailStart = 0
		}
		if err := copyFileTail(logPath, tailStart, rotated); err != nil {
			return err
		}
	}
	if err := os.Truncate(logPath, 0); err != nil {
		return fmt.Errorf("truncate dolt log %q: %w", logPath, err)
	}
	return nil
}

// copyFileTail copies srcPath from offset to EOF into destPath via a temp file
// and atomic rename, so a reader of destPath never observes a partial copy.
func copyFileTail(srcPath string, offset int64, destPath string) error {
	src, err := os.Open(srcPath) //nolint:gosec // path is the managed dolt log we own
	if err != nil {
		return fmt.Errorf("open dolt log %q for rotation: %w", srcPath, err)
	}
	defer src.Close() //nolint:errcheck // read-only handle
	if offset > 0 {
		if _, err := src.Seek(offset, io.SeekStart); err != nil {
			return fmt.Errorf("seek dolt log %q: %w", srcPath, err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(destPath), ".dolt-log-rot-*")
	if err != nil {
		return fmt.Errorf("create rotated dolt log temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("copy dolt log tail: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close rotated dolt log temp: %w", err)
	}
	if err := os.Rename(tmpName, destPath); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("install rotated dolt log %q: %w", destPath, err)
	}
	return nil
}

// managedDoltLogPathForCity resolves the managed dolt log path for a city from
// the supervisor's vantage point, where the per-city GC_DOLT_* environment is
// not set. It mirrors managedDoltOrderPackStateDir's resolution so it agrees
// with where startManagedDoltProcess writes the log.
func managedDoltLogPathForCity(cityPath string) string {
	packStateDir := managedDoltOrderPackStateDir(cityPath, nil)
	if strings.TrimSpace(packStateDir) == "" {
		return ""
	}
	return filepath.Join(packStateDir, "dolt.log")
}

// capManagedDoltLogsForCities caps the managed dolt log for each city path.
// Rotations are reported to stdout (operator-visible maintenance) and errors to
// stderr; one city's failure never aborts the others. Blank paths are skipped.
func capManagedDoltLogsForCities(cityPaths []string, maxBytes, keepTail int64, stdout, stderr io.Writer) {
	if maxBytes <= 0 {
		return
	}
	for _, cityPath := range cityPaths {
		if strings.TrimSpace(cityPath) == "" {
			continue
		}
		logPath := managedDoltLogPathForCity(cityPath)
		if logPath == "" {
			continue
		}
		capped, err := capManagedDoltLog(logPath, maxBytes, keepTail)
		if err != nil {
			if stderr != nil {
				fmt.Fprintf(stderr, "gc supervisor: city %q: cap dolt log: %v\n", cityPath, err) //nolint:errcheck
			}
			continue
		}
		if capped && stdout != nil {
			fmt.Fprintf(stdout, "Capped managed dolt log for city '%s' (exceeded %d bytes): %s\n", cityPath, maxBytes, logPath) //nolint:errcheck
		}
	}
}

// capRunningCityDoltLogs caps the managed dolt log of every running city in the
// registry. It is invoked from the supervisor patrol so the log stays bounded
// even in preserve mode, where the main dolt is never relaunched and would
// otherwise grow without limit for the city's whole lifetime.
func capRunningCityDoltLogs(cr *cityRegistry, stdout, stderr io.Writer) {
	if cr == nil {
		return
	}
	maxBytes, keepTail := resolveManagedDoltLogCap()
	if maxBytes <= 0 {
		return
	}
	snap := cr.Snapshot()
	if snap == nil {
		return
	}
	paths := make([]string, 0, len(snap.all))
	for _, v := range snap.all {
		if v == nil || !v.Started || strings.TrimSpace(v.Path) == "" {
			continue
		}
		paths = append(paths, v.Path)
	}
	capManagedDoltLogsForCities(paths, maxBytes, keepTail, stdout, stderr)
}
