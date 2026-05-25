#!/usr/bin/env bash
# reaper — close stale wisps with closed parents, purge old closed data, auto-close stale issues.
#
# Replaces mol-dog-reaper formula. All operations are deterministic:
# SQL queries with age thresholds, bd close/update commands, count
# comparisons against alert thresholds.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

CITY="${GC_CITY_PATH:-${GC_CITY:-.}}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$SCRIPT_DIR/dolt-target.sh"
CITY_ABS="$(cd "$CITY" 2>/dev/null && pwd -P || printf '%s\n' "$CITY")"
CITY_BEADS_DIR="$CITY_ABS/.beads"

# Configurable thresholds.
MAX_AGE="${GC_REAPER_MAX_AGE:-24h}"
PURGE_AGE="${GC_REAPER_PURGE_AGE:-168h}"
STALE_ISSUE_AGE="${GC_REAPER_STALE_ISSUE_AGE:-720h}"
SESSION_PURGE_AGE="${GC_REAPER_SESSION_PURGE_AGE:-720h}"
ALERT_THRESHOLD="${GC_REAPER_ALERT_THRESHOLD:-500}"
# Alert dedup: skip threshold-only escalations when the previous emit is
# fresher than ALERT_DEDUP_WINDOW_SEC and the open-wisp count has not moved
# by at least ALERT_DEDUP_DELTA. Without this, every cycle past the
# threshold spawns another mail wisp and feeds itself (ga-sh6).
ALERT_DEDUP_WINDOW_SEC="${GC_REAPER_ALERT_DEDUP_WINDOW_SEC:-3600}"
ALERT_DEDUP_DELTA="${GC_REAPER_ALERT_DEDUP_DELTA:-10}"
PACK_STATE_DIR="${GC_PACK_STATE_DIR:-${GC_CITY_RUNTIME_DIR:-$CITY/.gc/runtime}/packs/maintenance}"
ALERT_STATE_FILE="$PACK_STATE_DIR/reaper-alert-state.txt"
# Error dedup: a sustained DB failure (e.g. a broken rig DB) re-records the
# same error anomaly every cycle. Without dedup that floods the operator with
# one identical [MEDIUM] per reaper interval for the whole outage (ga-947).
# Emit once per ERROR_DEDUP_WINDOW_SEC for a given error signature; a changed
# or cleared signature re-emits immediately, and the window gives a periodic
# "still broken" heartbeat so a long outage is not silenced forever.
ERROR_DEDUP_WINDOW_SEC="${GC_REAPER_ERROR_DEDUP_WINDOW_SEC:-3600}"
ERROR_STATE_FILE="$PACK_STATE_DIR/reaper-error-state.txt"
DRY_RUN="${GC_REAPER_DRY_RUN:-}"

# Convert Go durations to SQL INTERVAL hours for Dolt.
duration_to_hours() {
    local dur="$1"
    # Strip trailing 'h' and return as integer.
    echo "${dur%h}"
}

MAX_AGE_H=$(duration_to_hours "$MAX_AGE")
PURGE_AGE_H=$(duration_to_hours "$PURGE_AGE")
STALE_AGE_H=$(duration_to_hours "$STALE_ISSUE_AGE")

CITY_DB_METADATA_RESULT=""

city_database_name() {
    local metadata="$CITY_BEADS_DIR/metadata.json"
    local db=""
    CITY_DB_METADATA_RESULT=""

    if [ -f "$metadata" ]; then
        if command -v jq >/dev/null 2>&1; then
            if ! db=$(jq -er '.dolt_database // empty | strings' "$metadata" 2>/dev/null); then
                return 0
            fi
        elif command -v python3 >/dev/null 2>&1; then
            if ! db=$(python3 - "$metadata" 2>/dev/null <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as f:
    value = json.load(f).get("dolt_database", "")
if isinstance(value, str) and value:
    print(value)
PY
            ); then
                return 0
            fi
        elif command -v grep >/dev/null 2>&1 && command -v sed >/dev/null 2>&1 && command -v head >/dev/null 2>&1; then
            if grep -q '}' "$metadata" 2>/dev/null; then
                db=$(grep -o '"dolt_database"[[:space:]]*:[[:space:]]*"[^"]*"' "$metadata" 2>/dev/null \
                    | sed 's/.*"dolt_database"[[:space:]]*:[[:space:]]*"//;s/"//' \
                    | head -1 || true)
            fi
        else
            return 0
        fi
    fi

    if [ -n "$db" ]; then
        CITY_DB_METADATA_RESULT="$db"
    fi
}

is_user_database() {
    case "$1" in
        information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe|benchdb|testdb_*|beads_pt*|beads_vr*|doctest_*|doctortest_*)
            return 1
            ;;
        beads_t*)
            local suffix="${1#beads_t}"
            if [[ "$suffix" =~ ^[0-9a-f]{8,}$ ]]; then
                return 1
            fi
            return 0
            ;;
        *)
            return 0
            ;;
    esac
}

# Discover databases from Dolt server. Exclude Dolt/MySQL system schemas,
# Gas City's internal health-probe database, and test-fixture scratch
# databases (benchdb, testdb_*, lowercase beads_t[0-9a-f]{8,}, beads_pt*,
# beads_vr*, doctest_*, doctortest_* — matching the Go cleanup planner
# contract); the remainder are bead stores.
DATABASES=$(
    while IFS= read -r db; do
        if is_user_database "$db"; then
            printf '%s\n' "$db"
        fi
    done < <(dolt_sql -r csv -q "SHOW DATABASES" 2>/dev/null | tail -n +2)
)
HAD_DATABASES=1
if [ -z "$DATABASES" ]; then
    # The Dolt-backed cleanup loop has no work, but the session-bead
    # prune below still operates through bd's configured task store.
    HAD_DATABASES=0
fi

TOTAL_STALE_WISPS=0
TOTAL_CLOSED_WISPS=0
TOTAL_PURGED=0
TOTAL_ISSUES_CLOSED=0
TOTAL_STALE_ISSUES_SKIPPED=0
TOTAL_SESSIONS_PRUNED=0
SESSION_PRUNE_ATTEMPTED=0
ANOMALIES=""
# ERROR_ANOMALIES accumulates only the non-threshold (error) anomalies — the
# subset that feeds the error-signature dedup. Threshold lines are appended to
# ANOMALIES directly and are deduped separately by open-wisp count.
ERROR_ANOMALIES=""

# Track which kind(s) of anomaly fired this run so the report stage can apply
# the right dedup: threshold anomalies dedup on open-wisp count, error
# (non-threshold) anomalies dedup on their signature. MAX_OPEN_WISPS holds the
# largest open-wisp count seen across databases this run; it feeds the
# threshold dedup delta comparison.
THRESHOLD_ANOMALY_RECORDED=0
NONTHRESHOLD_ANOMALY_RECORDED=0
MAX_OPEN_WISPS=0

sanitize_output() {
    printf '%s' "$1" | tr '\n' ' ' | cut -c1-500
}

record_anomaly() {
    local db="$1"
    shift
    ANOMALIES="${ANOMALIES}$db: $*
"
    ERROR_ANOMALIES="${ERROR_ANOMALIES}$db: $*
"
    NONTHRESHOLD_ANOMALY_RECORDED=1
}

# reaper_alert_should_emit decides whether the threshold (open-wisp count)
# component of an escalation is allowed to leave the script. It governs only
# the threshold class; error (non-threshold) anomalies are deduped separately
# by reaper_error_should_emit. Returns success (emit) when there is no prior
# state, the state is unparseable, the dedup window has expired, or the
# open-wisp count has moved by at least ALERT_DEDUP_DELTA. Returns failure
# (skip) only when a fresh prior emit had a close-enough count.
reaper_alert_should_emit() {
    local current_max="$1"
    [ -f "$ALERT_STATE_FILE" ] || return 0

    local last_epoch=""
    local last_count=""
    local key value
    while IFS='=' read -r key value; do
        case "$key" in
            epoch) last_epoch="$value" ;;
            count) last_count="$value" ;;
        esac
    done < "$ALERT_STATE_FILE"

    case "$last_epoch" in ''|*[!0-9]*) return 0 ;; esac
    case "$last_count" in ''|*[!0-9]*) return 0 ;; esac

    local now age diff
    now=$(date +%s 2>/dev/null || echo 0)
    case "$now" in ''|*[!0-9]*) return 0 ;; esac

    age=$((now - last_epoch))
    [ "$age" -lt 0 ] && age=0
    diff=$((current_max - last_count))
    [ "$diff" -lt 0 ] && diff=$((last_count - current_max))

    if [ "$age" -lt "$ALERT_DEDUP_WINDOW_SEC" ] && [ "$diff" -lt "$ALERT_DEDUP_DELTA" ]; then
        return 1
    fi
    return 0
}

# reaper_alert_save_state persists the most recent threshold-emit count and
# timestamp so future runs can dedup. Best-effort: a write failure does not
# fail the reaper run because the next cycle simply re-checks the threshold.
reaper_alert_save_state() {
    local current_max="$1"
    mkdir -p "$PACK_STATE_DIR" 2>/dev/null || return 0
    local now
    now=$(date +%s 2>/dev/null || echo 0)
    case "$now" in ''|*[!0-9]*) return 0 ;; esac
    printf 'epoch=%d\ncount=%d\n' "$now" "$current_max" > "$ALERT_STATE_FILE" 2>/dev/null || true
}

# reaper_error_signature derives a stable, single-line fingerprint of the
# accumulated error-anomaly text so repeated identical failures can be deduped.
# The exact algorithm is irrelevant — only that identical input yields the same
# fingerprint and different input differs. Prefer a real hash; fall back to a
# newline-stripped prefix when no hash tool is on PATH.
reaper_error_signature() {
    local text="$1"
    if command -v sha256sum >/dev/null 2>&1; then
        printf '%s' "$text" | sha256sum | cut -d' ' -f1
    elif command -v shasum >/dev/null 2>&1; then
        printf '%s' "$text" | shasum -a 256 | cut -d' ' -f1
    elif command -v cksum >/dev/null 2>&1; then
        printf '%s' "$text" | cksum | tr ' ' '_'
    else
        printf '%s' "$text" | tr '\n' ' ' | cut -c1-160
    fi
}

# reaper_error_should_emit decides whether the current set of error
# (non-threshold) anomalies should escalate. Emit when there is no prior
# state, the state is unparseable, the error signature changed (a new or
# different failure), or the dedup window has expired. Suppress only when an
# identical signature was emitted within ERROR_DEDUP_WINDOW_SEC — that is the
# sustained-outage flood case this dedup exists to silence (ga-947).
reaper_error_should_emit() {
    local current_sig="$1"
    [ -f "$ERROR_STATE_FILE" ] || return 0

    local last_epoch=""
    local last_sig=""
    local key value
    while IFS='=' read -r key value; do
        case "$key" in
            epoch) last_epoch="$value" ;;
            sig) last_sig="$value" ;;
        esac
    done < "$ERROR_STATE_FILE"

    case "$last_epoch" in ''|*[!0-9]*) return 0 ;; esac
    [ -n "$last_sig" ] || return 0
    [ "$current_sig" = "$last_sig" ] || return 0

    local now age
    now=$(date +%s 2>/dev/null || echo 0)
    case "$now" in ''|*[!0-9]*) return 0 ;; esac
    age=$((now - last_epoch))
    [ "$age" -lt 0 ] && age=0

    if [ "$age" -lt "$ERROR_DEDUP_WINDOW_SEC" ]; then
        return 1
    fi
    return 0
}

# reaper_error_save_state persists the most recent error-anomaly signature and
# timestamp so future runs can dedup. Best-effort, mirroring
# reaper_alert_save_state: a write failure does not fail the reaper run.
reaper_error_save_state() {
    local current_sig="$1"
    mkdir -p "$PACK_STATE_DIR" 2>/dev/null || return 0
    local now
    now=$(date +%s 2>/dev/null || echo 0)
    case "$now" in ''|*[!0-9]*) return 0 ;; esac
    printf 'epoch=%d\nsig=%s\n' "$now" "$current_sig" > "$ERROR_STATE_FILE" 2>/dev/null || true
}

CITY_DB_ANOMALY_RECORDED=0

valid_database_identifier() {
    local name="$1"

    case "$name" in
        ''|-*|*[!A-Za-z0-9_-]*)
            return 1
            ;;
    esac

    return 0
}

database_list_contains() {
    local needle="$1"
    local db

    while IFS= read -r db; do
        if [ "$db" = "$needle" ]; then
            return 0
        fi
    done <<EOF
$DATABASES
EOF

    return 1
}

CITY_DB=""
CITY_DB_SOURCE="$CITY_BEADS_DIR/metadata.json"
city_database_name
CITY_METADATA_DB="$CITY_DB_METADATA_RESULT"

if [ -n "${GC_REAPER_CITY_DATABASE:-}" ]; then
    CITY_DB_SOURCE="GC_REAPER_CITY_DATABASE"
    if [ -z "$CITY_METADATA_DB" ]; then
        record_anomaly "city" "city database $GC_REAPER_CITY_DATABASE from GC_REAPER_CITY_DATABASE could not be verified against $CITY_BEADS_DIR/metadata.json; stale issue auto-close disabled"
        CITY_DB_ANOMALY_RECORDED=1
    elif [ "$GC_REAPER_CITY_DATABASE" != "$CITY_METADATA_DB" ]; then
        record_anomaly "city" "city database $GC_REAPER_CITY_DATABASE from GC_REAPER_CITY_DATABASE does not match city metadata database $CITY_METADATA_DB; stale issue auto-close disabled"
        CITY_DB_ANOMALY_RECORDED=1
    else
        CITY_DB="$GC_REAPER_CITY_DATABASE"
    fi
else
    CITY_DB="$CITY_METADATA_DB"
fi

if [ -n "$CITY_DB" ] && ! valid_database_identifier "$CITY_DB"; then
    record_anomaly "city" "city database $CITY_DB from $CITY_DB_SOURCE is not a safe Dolt identifier; stale issue auto-close disabled"
    CITY_DB=""
    CITY_DB_ANOMALY_RECORDED=1
elif [ -n "$CITY_DB" ] && ! database_list_contains "$CITY_DB"; then
    record_anomaly "city" "city database $CITY_DB from $CITY_DB_SOURCE was not found in discovered databases; stale issue auto-close disabled"
    CITY_DB=""
    CITY_DB_ANOMALY_RECORDED=1
fi

SQL_COUNT_RESULT=0
get_sql_count() {
    local db="$1"
    local label="$2"
    local query="$3"
    local output
    local stderr_file
    local stderr_output
    local count

    SQL_COUNT_RESULT=0
    if ! stderr_file=$(mktemp); then
        record_anomaly "$db" "$label count failed for $db: could not create stderr capture file"
        return 0
    fi
    if ! output=$(dolt_sql -r csv -q "$query" 2>"$stderr_file"); then
        stderr_output=$(cat "$stderr_file" 2>/dev/null || true)
        rm -f "$stderr_file"
        record_anomaly "$db" "$label count failed for $db: $(sanitize_output "$output $stderr_output")"
        return 0
    fi
    rm -f "$stderr_file"

    count=$(printf '%s\n' "$output" | tail -1 | tr -d '\r')
    if [ -z "$count" ] || ! [[ "$count" =~ ^[0-9]+$ ]]; then
        record_anomaly "$db" "$label count returned non-numeric value for $db: $(sanitize_output "$output")"
        return 0
    fi

    SQL_COUNT_RESULT="$count"
}

SQL_ROWS_RESULT=""
get_sql_rows() {
    local db="$1"
    local label="$2"
    local query="$3"
    local output
    local stderr_file
    local stderr_output

    SQL_ROWS_RESULT=""
    if ! stderr_file=$(mktemp); then
        record_anomaly "$db" "$label query failed for $db: could not create stderr capture file"
        return 0
    fi
    if ! output=$(dolt_sql -r csv -q "$query" 2>"$stderr_file"); then
        stderr_output=$(cat "$stderr_file" 2>/dev/null || true)
        rm -f "$stderr_file"
        record_anomaly "$db" "$label query failed for $db: $(sanitize_output "$output $stderr_output")"
        return 0
    fi
    rm -f "$stderr_file"

    SQL_ROWS_RESULT=$(printf '%s\n' "$output" | tail -n +2 | tr -d '\r')
}

SQL_CHANGE_ROWS_RESULT=0
close_city_issue() {
    local issue_id="$1"
    local reason="$2"

    if [ ! -d "$CITY_BEADS_DIR" ]; then
        printf 'city bead store %s is unavailable' "$CITY_BEADS_DIR"
        return 1
    fi

    (
        cd "$CITY_ABS"
        BEADS_DIR="$CITY_BEADS_DIR" bd close "$issue_id" --reason "$reason"
    )
}

run_sql_change() {
    local db="$1"
    local label="$2"
    local query="$3"
    local output
    local rows
    local stderr_file
    local stderr_output

    SQL_CHANGE_ROWS_RESULT=0
    if ! stderr_file=$(mktemp); then
        record_anomaly "$db" "$label failed for $db: could not create stderr capture file"
        return 1
    fi
    if ! output=$(dolt_sql -r csv -q "
$query;
SELECT ROW_COUNT();
    " 2>"$stderr_file"); then
        stderr_output=$(cat "$stderr_file" 2>/dev/null || true)
        rm -f "$stderr_file"
        record_anomaly "$db" "$label failed for $db: $(sanitize_output "$output $stderr_output")"
        return 1
    fi
    stderr_output=$(cat "$stderr_file" 2>/dev/null || true)
    rm -f "$stderr_file"

    rows=$(printf '%s\n' "$output" | tail -1 | tr -d '\r')
    if [ -z "$rows" ] || ! [[ "$rows" =~ ^[0-9]+$ ]]; then
        record_anomaly "$db" "$label returned non-numeric row count for $db: $(sanitize_output "$output $stderr_output")"
        return 1
    fi

    SQL_CHANGE_ROWS_RESULT="$rows"
    return 0
}

# detect_dependency_columns inspects $1's `dependencies` table and sets
# DEP_WISP_COL / DEP_ISSUE_COL to the columns that hold, respectively, a
# wisp-typed and an issue-typed parent-child dependency target. bd <1.0.4
# stored every dependency target in one `depends_on_id` column; bd 1.0.4 split
# it into `depends_on_issue_id` / `depends_on_wisp_id` / `depends_on_external`.
# The reaper sweeps every bead store on one Dolt server and those stores may
# sit on different bd schema versions, so the column set is resolved per
# database. Defaults to the legacy single column; a probe failure leaves that
# default so the queries below fail loudly through the normal error path
# instead of silently skipping cleanup.
DEP_WISP_COL="depends_on_id"
DEP_ISSUE_COL="depends_on_id"
detect_dependency_columns() {
    local db="$1"
    local columns
    DEP_WISP_COL="depends_on_id"
    DEP_ISSUE_COL="depends_on_id"
    if ! columns=$(dolt_sql -r csv -q "SHOW COLUMNS FROM \`$db\`.dependencies" 2>/dev/null); then
        return 0
    fi
    if [[ $'\n'"$columns" == *$'\n'"depends_on_issue_id,"* ]]; then
        DEP_ISSUE_COL="depends_on_issue_id"
        DEP_WISP_COL="depends_on_wisp_id"
    fi
}

while IFS= read -r DB; do
    [ -z "$DB" ] && continue
    if ! valid_database_identifier "$DB"; then
        record_anomaly "$DB" "unsafe Dolt database identifier skipped by reaper"
        continue
    fi
    if ! has_wisps_table "$DB"; then
        # Not a bd-managed bead store. Skip silently; recording an
        # anomaly here would just turn every schemaless DB on the
        # server into noise. See gastownhall/gascity#1816.
        continue
    fi

    # Resolve the dependency-target column names for this database's bd schema
    # before building the parent-child queries below.
    detect_dependency_columns "$DB"

    DB_MUTATIONS=0

    # Step 1: Count stale non-closed wisps, then close only candidates whose
    # explicit parent-child edge points to a closed parent. Wisps
    # without a parent edge are reported but not closed by age alone.
    get_sql_count "$DB" "stale non-closed wisp" "
        SELECT COUNT(*) FROM \`$DB\`.wisps
        WHERE status IN ('open', 'hooked', 'in_progress')
        AND created_at < DATE_SUB(NOW(), INTERVAL $MAX_AGE_H HOUR)
    "
    STALE_WISP_COUNT=$SQL_COUNT_RESULT

    if [ "$STALE_WISP_COUNT" -gt 0 ]; then
        TOTAL_STALE_WISPS=$((TOTAL_STALE_WISPS + STALE_WISP_COUNT))
    fi

    CLOSE_WISP_COUNT=0
    DB_CLOSED_WISPS=0
    DB_PURGED=0
    while [ "$STALE_WISP_COUNT" -gt 0 ] && [ "$CLOSE_WISP_COUNT" -lt "$STALE_WISP_COUNT" ]; do
        get_sql_count "$DB" "schema-safe stale wisp" "
            SELECT COUNT(DISTINCT w.id) FROM \`$DB\`.wisps w
            INNER JOIN \`$DB\`.dependencies d
                ON d.issue_id = w.id
                AND d.type = 'parent-child'
            LEFT JOIN \`$DB\`.wisps parent_wisp ON d.$DEP_WISP_COL = parent_wisp.id
            LEFT JOIN \`$DB\`.issues parent_issue ON d.$DEP_ISSUE_COL = parent_issue.id
            WHERE w.status IN ('open', 'hooked', 'in_progress')
            AND w.created_at < DATE_SUB(NOW(), INTERVAL $MAX_AGE_H HOUR)
            AND (
                parent_wisp.status = 'closed'
                OR parent_issue.status = 'closed'
            )
        "
        CLOSE_WISP_BATCH=$SQL_COUNT_RESULT
        if [ "$CLOSE_WISP_BATCH" -eq 0 ] || [ -n "$DRY_RUN" ]; then
            break
        fi

        if run_sql_change "$DB" "closing stale wisps" "
            UPDATE \`$DB\`.wisps SET status='closed', closed_at=NOW()
            WHERE status IN ('open', 'hooked', 'in_progress')
            AND created_at < DATE_SUB(NOW(), INTERVAL $MAX_AGE_H HOUR)
            AND id IN (
                SELECT id FROM (
                    SELECT w.id FROM \`$DB\`.wisps w
                    INNER JOIN \`$DB\`.dependencies d
                        ON d.issue_id = w.id
                        AND d.type = 'parent-child'
                    LEFT JOIN \`$DB\`.wisps parent_wisp ON d.$DEP_WISP_COL = parent_wisp.id
                    LEFT JOIN \`$DB\`.issues parent_issue ON d.$DEP_ISSUE_COL = parent_issue.id
                    WHERE w.status IN ('open', 'hooked', 'in_progress')
                    AND w.created_at < DATE_SUB(NOW(), INTERVAL $MAX_AGE_H HOUR)
                    AND (
                        parent_wisp.status = 'closed'
                        OR parent_issue.status = 'closed'
                    )
                ) reaper_wisp_candidates
            )
        "; then
            CLOSE_WISP_ROWS=$SQL_CHANGE_ROWS_RESULT
            if [ "$CLOSE_WISP_ROWS" -eq 0 ]; then
                break
            fi
            CLOSE_WISP_COUNT=$((CLOSE_WISP_COUNT + CLOSE_WISP_ROWS))
            DB_CLOSED_WISPS=$((DB_CLOSED_WISPS + CLOSE_WISP_ROWS))
            TOTAL_CLOSED_WISPS=$((TOTAL_CLOSED_WISPS + CLOSE_WISP_ROWS))
            DB_MUTATIONS=$((DB_MUTATIONS + CLOSE_WISP_ROWS))
        else
            break
        fi
    done

    # Step 2: Purge — delete closed wisps past purge_age.
    get_sql_count "$DB" "closed wisp purge" "
        SELECT COUNT(*) FROM \`$DB\`.wisps
        WHERE status = 'closed'
        AND closed_at < DATE_SUB(NOW(), INTERVAL $PURGE_AGE_H HOUR)
        AND id NOT IN (
            SELECT DISTINCT d.$DEP_WISP_COL FROM \`$DB\`.dependencies d
            INNER JOIN \`$DB\`.wisps child_wisp ON d.issue_id = child_wisp.id
            WHERE d.type = 'parent-child'
            AND d.$DEP_WISP_COL IS NOT NULL
            AND child_wisp.status IN ('open', 'hooked', 'in_progress')
        )
    "
    PURGE_COUNT=$SQL_COUNT_RESULT

    if [ "$PURGE_COUNT" -gt 0 ] && [ -z "$DRY_RUN" ]; then
        if run_sql_change "$DB" "purging closed wisps" "
            DELETE FROM \`$DB\`.wisps
            WHERE status = 'closed'
            AND closed_at < DATE_SUB(NOW(), INTERVAL $PURGE_AGE_H HOUR)
            AND id NOT IN (
                SELECT DISTINCT d.$DEP_WISP_COL FROM \`$DB\`.dependencies d
                INNER JOIN \`$DB\`.wisps child_wisp ON d.issue_id = child_wisp.id
                WHERE d.type = 'parent-child'
                AND d.$DEP_WISP_COL IS NOT NULL
                AND child_wisp.status IN ('open', 'hooked', 'in_progress')
            )
        "; then
            PURGED_ROWS=$SQL_CHANGE_ROWS_RESULT
            DB_PURGED=$((DB_PURGED + PURGED_ROWS))
            TOTAL_PURGED=$((TOTAL_PURGED + PURGED_ROWS))
            DB_MUTATIONS=$((DB_MUTATIONS + PURGED_ROWS))
        fi
    fi

    # Step 4: Auto-close stale issues (exclude P0/P1, epics, active deps).
    DB_ISSUES_CLOSED=0
    get_sql_rows "$DB" "stale issue" "
        SELECT id FROM \`$DB\`.issues
        WHERE status IN ('open', 'in_progress')
        AND updated_at < DATE_SUB(NOW(), INTERVAL $STALE_AGE_H HOUR)
        AND priority > 1
        AND issue_type != 'epic'
        AND id NOT IN (
            SELECT DISTINCT d.issue_id FROM \`$DB\`.dependencies d
            INNER JOIN \`$DB\`.issues i ON d.$DEP_ISSUE_COL = i.id
            WHERE i.status IN ('open', 'in_progress')
            UNION
            SELECT DISTINCT d.$DEP_ISSUE_COL FROM \`$DB\`.dependencies d
            INNER JOIN \`$DB\`.issues i ON d.issue_id = i.id
            WHERE i.status IN ('open', 'in_progress')
        )
    "
    STALE_IDS=$SQL_ROWS_RESULT

    if [ -n "$STALE_IDS" ] && [ -z "$DRY_RUN" ]; then
        if [ -z "$CITY_DB" ]; then
            if [ "$CITY_DB_ANOMALY_RECORDED" -eq 0 ]; then
                record_anomaly "city" "city database could not be determined from GC_REAPER_CITY_DATABASE or $CITY/.beads/metadata.json; stale issue auto-close disabled"
                CITY_DB_ANOMALY_RECORDED=1
            fi
            SKIPPED_ISSUES=$(printf '%s\n' "$STALE_IDS" | sed '/^[[:space:]]*$/d' | wc -l | tr -d ' ')
            TOTAL_STALE_ISSUES_SKIPPED=$((TOTAL_STALE_ISSUES_SKIPPED + SKIPPED_ISSUES))
        elif [ "$DB" != "$CITY_DB" ]; then
            SKIPPED_ISSUES=$(printf '%s\n' "$STALE_IDS" | sed '/^[[:space:]]*$/d' | wc -l | tr -d ' ')
            TOTAL_STALE_ISSUES_SKIPPED=$((TOTAL_STALE_ISSUES_SKIPPED + SKIPPED_ISSUES))
        else
            while IFS= read -r issue_id; do
                [ -z "$issue_id" ] && continue
                if CLOSE_OUTPUT=$(close_city_issue "$issue_id" "stale:auto-closed by reaper" 2>&1); then
                    DB_ISSUES_CLOSED=$((DB_ISSUES_CLOSED + 1))
                    TOTAL_ISSUES_CLOSED=$((TOTAL_ISSUES_CLOSED + 1))
                    DB_MUTATIONS=$((DB_MUTATIONS + 1))
                else
                    record_anomaly "$DB" "closing stale issue $issue_id failed for $DB: $(sanitize_output "$CLOSE_OUTPUT")"
                fi
            done <<< "$STALE_IDS"
        fi
    fi

    # Step 5: Anomaly check — open wisp count.
    get_sql_count "$DB" "open wisp" "
        SELECT COUNT(*) FROM \`$DB\`.wisps
        WHERE status IN ('open', 'hooked', 'in_progress')
    "
    OPEN_WISPS=$SQL_COUNT_RESULT

    if [ "$OPEN_WISPS" -gt "$ALERT_THRESHOLD" ]; then
        ANOMALIES="${ANOMALIES}$DB: $OPEN_WISPS open wisps (threshold: $ALERT_THRESHOLD)\n"
        THRESHOLD_ANOMALY_RECORDED=1
        if [ "$OPEN_WISPS" -gt "$MAX_OPEN_WISPS" ]; then
            MAX_OPEN_WISPS=$OPEN_WISPS
        fi
    fi

    # Commit Dolt changes. Must use CALL (not SELECT) and have an active
    # database via USE so CALL DOLT_COMMIT(...) runs in the target database.
    # Commit failures are surfaced as anomalies so the dog loop does not
    # silently retry forever.
    if [ -z "$DRY_RUN" ] && [ "$DB_MUTATIONS" -gt 0 ]; then
        if ! COMMIT_OUTPUT=$(dolt_sql -q "
            USE \`$DB\`;
            CALL DOLT_COMMIT('-Am', 'reaper: stale_wisps=$STALE_WISP_COUNT closed_wisps=$DB_CLOSED_WISPS purged=$DB_PURGED stale_issues=$DB_ISSUES_CLOSED', '--author', 'reaper <reaper@gastown.local>')
        " 2>&1); then
            case "$COMMIT_OUTPUT" in
                *"nothing to commit"*|*"Nothing to commit"*)
                    :
                    ;;
                *)
                    record_anomaly "$DB" "Dolt commit failed for $DB: $(sanitize_output "$COMMIT_OUTPUT")"
                    ;;
            esac
        fi
    fi
done <<EOF
$DATABASES
EOF

# Step 6: prune closed gm session beads from the city's primary bead store.
if [ -d "$CITY_BEADS_DIR" ] && command -v bd >/dev/null 2>&1; then
    SESSION_PRUNE_ATTEMPTED=1
    BD_PRUNE_ARGS=(prune --pattern 'gm-*' --older-than "$SESSION_PURGE_AGE")
    if [ -z "$DRY_RUN" ]; then
        BD_PRUNE_ARGS+=(--force)
    fi
    BD_PRUNE_ARGS+=(--json)

    if PRUNE_JSON=$((
        cd "$CITY_ABS" && BEADS_DIR="$CITY_BEADS_DIR" bd "${BD_PRUNE_ARGS[@]}"
    ) 2>/dev/null); then
        :
    else
        PRUNE_JSON='{"pruned_count":0}'
    fi
    PRUNE_COUNT=$(printf '%s' "$PRUNE_JSON" | sed -n 's/.*"pruned_count"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' | head -1)
    [ -z "$PRUNE_COUNT" ] && PRUNE_COUNT=0
    TOTAL_SESSIONS_PRUNED=$PRUNE_COUNT
    if [ "$PRUNE_COUNT" -gt 1000 ]; then
        record_anomaly "gm" "$PRUNE_COUNT closed session beads pruned in one run (threshold: 1000)"
    fi
fi

if [ "$HAD_DATABASES" -eq 0 ] && [ "$SESSION_PRUNE_ATTEMPTED" -eq 0 ]; then
    exit 0
fi

# Report. The two anomaly classes dedup independently, then the combined
# report is mailed if EITHER class wants out. Threshold anomalies dedup on the
# open-wisp count (reaper_alert_should_emit); error anomalies dedup on a
# signature of their text (reaper_error_should_emit) so a sustained DB failure
# that re-records the same error every cycle mails once per window instead of
# every cycle (ga-947). A changed or cleared error signature re-emits at once.
if [ -n "$ANOMALIES" ]; then
    EMIT_THRESHOLD=0
    EMIT_ERROR=0
    ERROR_SIG=""

    if [ "$THRESHOLD_ANOMALY_RECORDED" -eq 1 ]; then
        if reaper_alert_should_emit "$MAX_OPEN_WISPS"; then
            EMIT_THRESHOLD=1
        fi
    fi

    if [ "$NONTHRESHOLD_ANOMALY_RECORDED" -eq 1 ]; then
        ERROR_SIG=$(reaper_error_signature "$ERROR_ANOMALIES")
        if reaper_error_should_emit "$ERROR_SIG"; then
            EMIT_ERROR=1
        fi
    fi

    if [ "$EMIT_THRESHOLD" -eq 1 ] || [ "$EMIT_ERROR" -eq 1 ]; then
        gc mail send mayor/ -s "ESCALATION: Reaper anomalies detected [MEDIUM]" \
            -m "$ANOMALIES" 2>/dev/null || true
        # Refresh dedup state for every class present in this emitted report,
        # not just the class that triggered it: the operator has now seen both,
        # so restart both windows. Otherwise an error-driven emit would leave
        # the threshold window stale (and vice versa) and the next cycle would
        # re-mail the un-refreshed class immediately.
        if [ "$THRESHOLD_ANOMALY_RECORDED" -eq 1 ]; then
            reaper_alert_save_state "$MAX_OPEN_WISPS"
        fi
        if [ "$NONTHRESHOLD_ANOMALY_RECORDED" -eq 1 ]; then
            reaper_error_save_state "$ERROR_SIG"
        fi
    fi
fi

SUMMARY="reaper — stale_wisps:$TOTAL_STALE_WISPS, closed_wisps:$TOTAL_CLOSED_WISPS, purged:$TOTAL_PURGED, sessions-pruned:$TOTAL_SESSIONS_PRUNED, closed:$TOTAL_ISSUES_CLOSED, skipped_non_city_issues:$TOTAL_STALE_ISSUES_SKIPPED"
if [ -n "$DRY_RUN" ]; then
    SUMMARY="$SUMMARY (dry run)"
fi

gc session nudge deacon/ "DOG_DONE: $SUMMARY" 2>/dev/null || true
echo "reaper: $SUMMARY"
