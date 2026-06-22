---
title: Dolt CPU Storm Forensics
description: Capture a forensic trail before the dolt-cpu-restart break-glass restarts a CPU-pegged managed Dolt server.
---

## Overview

Gas City runs beads on a managed Dolt SQL server. If that server pegs a CPU
core for a sustained period, the dipcity `dolt-cpu-restart` break-glass
(`assets/scripts/dolt-cpu-restart.sh`, gated by
`orders/dolt-cpu-restart-cooldown.toml`) restarts it to recover throughput.

A bare restart recovers availability but **destroys the evidence**: the next
server starts clean and the storm's cause is lost. The known acute triggers
are already mitigated — auto-GC and the persistent stats worker are disabled
in the managed config (`gc dolt-config write-managed` /
`writeManagedDoltConfigFile`), and the commit-storm trigger was removed — so a
*recurrence* is, by definition, something not yet understood. This runbook
documents the off-by-default forensic toggles to arm **before** a restart so
the recurrence can be diagnosed.

## Verify the baseline first

The managed server should already have auto-GC and the stats worker off. A
storm with these *on* is a config-drift bug, not a new cause. Confirm with a
read-only query:

```bash
gc dolt sql -q "SELECT @@dolt_auto_gc_enabled, @@dolt_stats_enabled"
# Expect: OFF, OFF
```

If either is `ON`, the managed config drifted — re-run
`gc dolt-config write-managed` (the supervisor does this on each start) and
treat the drift as the root cause before going further.

## Forensic toggles (gascity side)

Both are **off by default** and survive a supervisor/launchd restart via the
service env allowlist (`supervisorServiceEnvKeys`). On macOS, set them with
`launchctl setenv` so the launchd-domain value reaches the managed server even
when `gc start` runs from a plain shell (`providerLifecycleProcessEnv` reads
them back via `launchctl getenv`).

| Env var | Effect | Captures |
| --- | --- | --- |
| `GC_DOLT_LOGLEVEL` | Sets the managed config `log_level` (`trace`/`debug`/…). | Dolt's internal query/engine log lines on the **next** server lifetime. |
| `GC_DOLT_METRICS_PORT` | Emits a `metrics` block bound to `127.0.0.1` only. | Dolt's Prometheus gauges — `dss_concurrent_queries`, `dss_concurrent_connections` — scrapeable live during a storm. |

`GC_DOLT_METRICS_PORT` binds the listener to `127.0.0.1` regardless of the SQL
listener host, so the diagnostic port is never reachable off-host. A value of
`<= 0` (or anything non-numeric) disables it. Because both toggles take effect
at server start, they capture the **next** lifetime — arm them, let the storm
recur, then capture before the restart.

## Capture-then-restart flow

The dipcity break-glass performs the capture; this is the order of operations
it follows when sustained CPU is detected.

1. **Goroutine snapshot of the *current* storm (no pre-arming needed).** Send
   `SIGQUIT` to the dolt PID. Go's runtime prints every goroutine stack —
   including the ones on-CPU right now — to the dolt log. Sample it 2–3 times a
   few seconds apart to see which stacks are consistently running.

   ```bash
   pid=$(pgrep -f 'dolt sql-server' | head -1)
   kill -QUIT "$pid"   # stack dump lands in the managed dolt log; dolt keeps running
   ```

2. **Scrape the metrics port if armed.** Distinguishes a query flood (gauges
   high) from background work (gauges low while CPU is high).

   ```bash
   curl -s "http://127.0.0.1:${GC_DOLT_METRICS_PORT}/metrics" \
     | grep -E 'dss_concurrent_(queries|connections)'
   ```

3. **Optional sampling CPU profile of the current storm** (Linux, if `perf` is
   available): `perf record -g -p "$pid" -- sleep 20`.

4. **Arm the toggles for the next lifetime**, then **restart gracefully** so
   nothing in flight is lost:

   ```bash
   launchctl setenv GC_DOLT_LOGLEVEL trace        # macOS; or export for the supervisor env
   launchctl setenv GC_DOLT_METRICS_PORT 51913
   gc dolt restart                                 # SIGTERM→SIGKILL grace, not a bare kill
   ```

## Disarming

Forensic mode is opt-in and should not stay on indefinitely — verbose logging
and a live metrics listener both carry a small standing cost. Once the trail is
captured, clear the toggles and restart:

```bash
launchctl unsetenv GC_DOLT_LOGLEVEL
launchctl unsetenv GC_DOLT_METRICS_PORT
gc dolt restart
```

Verify the metrics listener is gone:

```bash
ss -tlnp | grep 127.0.0.1:51913 || echo "metrics listener closed"
```

## See also

- `docs/troubleshooting/dolt-bloat-recovery.md` — recovering an oversized store.
- `assets/scripts/dolt-cpu-restart.sh` (dipcity) — the break-glass that drives
  this capture.
