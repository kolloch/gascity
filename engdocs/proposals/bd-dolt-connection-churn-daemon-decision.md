---
title: "bd → dolt Connection Churn: Daemon Decision (Measure-then-Decide)"
---

## Decision

**Do NOT build the persistent `bd` daemon (pe-3xzd Shape A) at this time.**
The per-command subprocess churn that originally motivated it no longer
pegs dolt after the mitigations shipped. Of the three options the parent
bead (`ga-7tp5`) framed — (A) daemon, (B) batch-stdin only, (C) accept —
the answer is **(C) accept the current architecture**, plus one targeted
follow-up to fix a localized N+1 in `gc mail inbox`.

This is a measure-then-decide record, not an implementation. It exists so
the next agent (or the operator) revisiting "should we build the bd daemon?"
can see the evidence without re-running the measurements.

## Context

`pe-3xzd` proposed two shapes to remove `bd → dolt` connection churn:

- **Shape A** — a persistent `gc bd-daemon` over a Unix socket that reuses
  one dolt connection for all `gc bd` / `gc mail` calls.
- **Shape B** — a `bd batch --stdin` mode for loop-y scripts.

Its premise: "multiple agents polling, plus orders that iterate per-bead,
produce sustained 60+ connections/sec to dolt and saturate its thread
pool." Since then three things changed the cost picture:

1. **`ga-sc9`** (merged) — auto-injects `--readonly --dolt-auto-commit=off`
   for known read-only subcommands, killing the implicit-COMMIT spam.
2. **`ga-blt`** (merged) + 6h cooldown — `wisp-compact` now batches its bd
   ops in one subprocess instead of 5k–32k per-bead spawns (the ×4000
   amplifier behind the original dolt CPU peg).
3. **`pe-jllvd` / `ga-qgtv`** (merged) — Dolt journal GC, which had been
   making *every* bd call thread through a bloated journal.

Plus `ga-a60` / `ga-qq8` (merged) batched the mail inbox/target-resolution
path. `ga-7tp5` is the re-measure-and-decide bead.

## Method

Measured on the live city under normal multi-agent load (not a synthetic
quiescent box):

- Host: 16 cores, load avg ~2.75, ~7 days uptime.
- dolt: managed `dolt sql-server` on `127.0.0.1:50215`,
  `max_connections: 1000`, `auto_gc`/stats disabled (per the managed
  config).
- Pool/churn: `ss -tn` sampling of ESTAB and TIME-WAIT sockets to `:50215`.
- dolt CPU: `/proc/<pid>/stat` utime+stime delta over a 5s window.
- Latency: 15 iterations per call path, wall-clock, p50/p95.
- Round-trips: `strace -f -e trace=connect` counting `connect()` to
  `:50215` per command (isolates one process from concurrent agent noise,
  which makes TIME-WAIT deltas unreliable).

## Findings

### Connection pool is nowhere near saturation

| Metric | Observed | Ceiling | Utilization |
| --- | --- | --- | --- |
| Concurrent ESTAB to `:50215` | 2–4 | 1000 | **~0.2–0.4%** |
| Ambient churn (TIME-WAIT ≈ 1.6k / 60s) | **~27 conn/sec** | — | below the 62/sec wisp-compact peak that is now gone |
| dolt CPU (5s sample) | **~22% of one core** | 1600% (16 cores) | not pegged |

The pre-fix worst case (`pe-4lpn`: 62 new conn/sec, "exhaust in ~16s")
no longer reproduces. High TIME-WAIT accumulation (~1.6k) confirms churn
is real, but it is harmless given 99.8% pool headroom and an idle thread
pool.

### Per-call latency — one outlier

| Call path | p50 | p95 |
| --- | --- | --- |
| `bd` read fast-path (post `ga-sc9`) | 59 ms | 77 ms |
| `gc bd ready --json` | 124 ms | 139 ms |
| `gc bd show --json` | 178 ms | 208 ms |
| **`gc mail inbox`** | **3726 ms** | **4133 ms** |

`bd` cold-open is ~60 ms (matches the `ga-1rog` estimate). The `gc`
wrapper adds ~2–3× over raw `bd`. `gc mail inbox` is a **20–60× outlier**.

### `gc mail inbox` is still N+1 after the batch fix

`strace` connect-count per command (isolated):

| Command | `connect(:50215)` | bd subprocess spawns |
| --- | --- | --- |
| `gc bd list --limit 5` | 8 | — |
| `gc bd ready --limit 5` | 8 | — |
| **`gc mail inbox`** | **44** | **10** |

Binary A/B of `gc mail inbox` latency:

| Binary | Date | Mail fix? | median |
| --- | --- | --- | --- |
| `~/.local/bin/gc` 1.1.1 | May 20 | no | ~4.3 s |
| `~/go/bin/gc` 1.1.1 (city) | Jun 15 | yes | ~3.8 s |
| current `main` build | Jun 22 | yes | ~3.8 s |

`ga-a60` did help (its bead notes "~17s"; now ~3.8s) but did **not** close
the gap to `gc bd list` (~0.13s). The residual cost is ~44 dolt
round-trips and ~10 `bd` subprocess spawns per inbox poll — an N+1, not a
connection-setup problem.

## Rationale

1. **Churn no longer pegs dolt.** 0.2% pool utilization, ~22% CPU, ~27
   conn/sec — all far below threshold. The saturation premise of `pe-3xzd`
   was resolved by `ga-sc9` + `ga-blt` + journal GC. Building a daemon to
   relieve pressure that no longer exists fails the cost/benefit test.

2. **A daemon targets the wrong cost.** Shape A amortizes connection
   *setup*. On loopback the TCP handshake is sub-millisecond; the ~60 ms is
   `bd` *process* cold-open + query, not socket setup. The only real
   latency hotspot (`gc mail inbox`) is ~44 serial *queries* and ~10
   subprocess spawns — a daemon reusing one connection would still issue
   all 44 queries. The correct fix is cutting round-trips at the source,
   which is what `ga-a60` began and a targeted follow-up should finish.

3. **A persistent sidecar is a large, principle-conflicting investment.**
   It adds a long-lived child process, a socket protocol, lifecycle and
   crash-recovery machinery — against AGENTS.md "no premature abstraction"
   and the "query live state, no status files / no extra daemons" grain.
   Not warranted when a localized fix addresses the only hotspot.

4. **Shape B is already in flight where it helps.** `ga-blt` batched the
   worst loop-y offender (`wisp-compact`); `ga-lyf3` (open) extends the
   `bd batch` grammar for the rest. No daemon needed for the script case.

## Follow-ups

- **NEW (filed in gascity):** fix the residual `gc mail inbox` N+1 — ~44
  dolt round-trips + ~10 `bd` subprocess spawns per call → target a single
  batched query path (< ~0.5 s), matching `gc bd list`. This is the highest
  user-facing win and is a small, localized fix (not the large daemon
  spend).
- **`ga-lyf3`** (open) — continue Shape B `bd batch` grammar for scripts.
- **Ops note:** the city runs the Jun-15 `~/go/bin/gc`; `~/.local/bin/gc`
  is a stale May-20 build. Worth aligning so shell measurements match
  deployed behavior.

## Re-measure trigger

Revisit Shape A only if agent growth pushes any of:

- concurrent ESTAB to `:50215` sustained above ~250 (25% of pool), or
- dolt CPU sustained > 80% of a core attributable to connection setup, or
- ambient churn back above ~60 conn/sec.

Until then, the architecture is accepted as-is.
