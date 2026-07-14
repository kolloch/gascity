package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Default rotation tunables. Operators can override these via the
// functional options below; the defaults match the architect's NFRs
// and were chosen so a busy city rotates roughly once per day at
// steady-state throughput.
const (
	defaultRotationMaxSize       = 256 * 1024 * 1024 // 256 MiB
	defaultRotationCheckRecords  = 1024
	defaultRotationCheckInterval = 60 * time.Second
)

// FileRecorder appends events to a JSONL file. It uses O_APPEND for
// cross-process safety and a mutex for in-process serialization.
// Recording errors are written to stderr and never returned.
//
// FileRecorder implements [Provider] — it can both record and read events.
type FileRecorder struct {
	mu     sync.Mutex
	path   string
	file   *os.File
	seq    uint64
	stderr io.Writer
	closed bool

	// rotations tracks in-flight rotation goroutines so Close can
	// drain them. Without this, callers that read events.jsonl
	// immediately after Close() can miss events that are still in
	// rotating-* files awaiting gzip+rename.
	rotations sync.WaitGroup

	// Rotation tunables. Zero MaxSize disables size-triggered
	// rotation; ForceRotate continues to work regardless. The check
	// fields amortize the cost of stat-ing the active file: Record
	// only consults size when at least one of (recordCount %
	// rotationCheckRecords == 0) or (now - lastSizeCheck >=
	// rotationCheckInterval) holds.
	maxSize               int64
	rotationCheckRecords  int
	rotationCheckInterval time.Duration
	archiveRetainAge      time.Duration
	recordCount           uint64
	lastSizeCheck         time.Time
}

// FileRecorderOption customizes a FileRecorder at construction time.
// Use With* helpers to set specific tunables; an unmodified recorder
// keeps the defaults documented above.
type FileRecorderOption func(*FileRecorder)

// WithMaxSize sets the size threshold (in bytes) above which Record
// auto-rotates the active log. A non-positive value disables
// size-triggered rotation; ForceRotate continues to work.
func WithMaxSize(bytes int64) FileRecorderOption {
	return func(r *FileRecorder) { r.maxSize = bytes }
}

// WithRotationCheckRecords sets how often (in records) Record checks
// the active file's size against MaxSize. A larger interval reduces
// stat syscalls at the cost of overshooting the threshold by up to
// one window of records. Defaults to 1024.
func WithRotationCheckRecords(n int) FileRecorderOption {
	return func(r *FileRecorder) { r.rotationCheckRecords = n }
}

// WithRotationCheckInterval sets the time-based backstop for size
// checks: even on low-traffic cities that never reach
// rotationCheckRecords, Record will stat the active file at least
// once per interval. Defaults to 60s.
func WithRotationCheckInterval(d time.Duration) FileRecorderOption {
	return func(r *FileRecorder) { r.rotationCheckInterval = d }
}

// WithArchiveRetainAge sets the maximum age of canonical archive
// files kept after a successful rotation. A non-positive value keeps
// all archives forever.
func WithArchiveRetainAge(d time.Duration) FileRecorderOption {
	return func(r *FileRecorder) { r.archiveRetainAge = d }
}

// RotationResult is returned by ForceRotate (and B-3's API endpoint)
// describing the outcome of a single rotation. Field-stable contract:
// downstream wire layers depend on these names.
type RotationResult struct {
	// Rotated is true when an archive was produced; false on the
	// no-op path (empty active log).
	Rotated bool

	// Reason is populated only when Rotated is false; it explains
	// why the rotation was skipped.
	Reason string

	// ArchivePath is the absolute path to the canonical .gz archive
	// that this rotation produced. Empty when Rotated is false.
	ArchivePath string

	// FirstSeq, LastSeq is the seq window covered by the archive,
	// inclusive on both ends.
	FirstSeq uint64
	LastSeq  uint64

	// AnchorSeq is the seq of the events.rotated event written as
	// the first record of the new active log.
	AnchorSeq uint64

	// AnchorTimestamp is the timestamp on the anchor event.
	AnchorTimestamp time.Time

	// CompressionPending is true on success: the rename of the old
	// active file is synchronous, but gzip compression runs in a
	// background goroutine. Use Done to wait for completion.
	CompressionPending bool

	// Done is closed when the background gzip + rename completes
	// (whether the gzip itself succeeded or failed). Nil when
	// Rotated is false. Not serialized on the wire.
	Done <-chan struct{} `json:"-"`
}

// NewFileRecorder opens (or creates) the event log at path. It reads the tail
// sequence from any existing append-only log so new events continue
// monotonically. Parent directories are created as needed. Optional
// FileRecorderOption values configure rotation behavior; defaults
// are documented on each option.
//
// On open, the constructor performs a one-shot sweep on the log
// directory: legacy events.jsonl.archive-YYYYMMDD.gz files are
// renamed to the seq-stamped convention using the migration time as
// their retention timestamp, events.jsonl.rotating-* files left from a
// crashed rotation are gzipped into canonical archive names, and
// *.gz.tmp files are removed. Sweep failures are logged to stderr and
// do not block the recorder from opening.
func NewFileRecorder(path string, stderr io.Writer, opts ...FileRecorderOption) (*FileRecorder, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating event log directory: %w", err)
	}

	if err := reapOrphanedRotatingFiles(filepath.Dir(path), stderr); err != nil {
		fmt.Fprintf(stderr, "events: rotation: orphan sweep: %v\n", err) //nolint:errcheck // best-effort stderr
	}

	maxSeq, err := ReadLatestSeq(path)
	if err != nil {
		return nil, err
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening event log: %w", err)
	}

	r := &FileRecorder{
		path:                  path,
		file:                  file,
		seq:                   maxSeq,
		stderr:                stderr,
		maxSize:               0,
		rotationCheckRecords:  defaultRotationCheckRecords,
		rotationCheckInterval: defaultRotationCheckInterval,
		lastSizeCheck:         time.Now(),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// Record appends an event to the log. It auto-fills Seq and Ts (if zero).
// Errors are written to stderr — never returned.
//
// Records are gated on size: when the recorder is configured with a
// non-zero MaxSize, Record may rotate the active log before writing
// if the file has crossed the threshold since the last check. Auto
// rotation is amortized — see WithRotationCheckRecords / Interval.
func (r *FileRecorder) Record(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return
	}

	r.maybeAutoRotateLocked()

	if err := syscall.Flock(int(r.file.Fd()), syscall.LOCK_EX); err != nil {
		fmt.Fprintf(r.stderr, "events: lock: %v\n", err) //nolint:errcheck // best-effort stderr
		return
	}
	defer func() {
		if err := syscall.Flock(int(r.file.Fd()), syscall.LOCK_UN); err != nil {
			fmt.Fprintf(r.stderr, "events: unlock: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}()

	if err := r.writeRecordLocked(&e); err != nil {
		fmt.Fprintf(r.stderr, "events: %v\n", err) //nolint:errcheck // best-effort stderr
	}
}

// writeRecordLocked appends e to the active log under the recorder
// mutex. Auto-fills Seq and Ts (if zero). The caller must already
// hold both r.mu and (if cross-process safety matters) the file's
// flock. Returns an error on marshal or write failure; the caller
// decides whether to log to stderr or surface it.
func (r *FileRecorder) writeRecordLocked(e *Event) error {
	if latest, err := readLatestActiveSeq(r.path); err == nil && latest > r.seq {
		r.seq = latest
	} else if err != nil {
		return fmt.Errorf("latest seq: %w", err)
	}
	r.seq++
	e.Seq = r.seq
	if e.Ts.IsZero() {
		e.Ts = time.Now()
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	data = append(data, '\n')
	if _, err := r.file.Write(data); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	r.recordCount++
	return nil
}

// maybeAutoRotateLocked is the size-gated rotation hook on the
// Record() hot path. It returns immediately if size-triggered
// rotation is disabled (MaxSize <= 0) or if neither the
// records-since-check nor the time-since-check threshold has been
// crossed. On a check, it stats the active file and triggers
// rotateLocked if size has exceeded MaxSize.
//
// Rotation failures are logged to stderr — Record's contract is
// best-effort and a failed rotation must not block subsequent
// writes. The next Record call will retry.
func (r *FileRecorder) maybeAutoRotateLocked() {
	if r.maxSize <= 0 {
		return
	}
	checkRecords := r.rotationCheckRecords
	if checkRecords <= 0 {
		checkRecords = defaultRotationCheckRecords
	}
	checkInterval := r.rotationCheckInterval
	if checkInterval <= 0 {
		checkInterval = defaultRotationCheckInterval
	}
	if r.recordCount%uint64(checkRecords) != 0 && time.Since(r.lastSizeCheck) < checkInterval {
		return
	}
	r.lastSizeCheck = time.Now()

	info, err := r.file.Stat()
	if err != nil {
		fmt.Fprintf(r.stderr, "events: rotation: size check: %v\n", err) //nolint:errcheck // best-effort stderr
		return
	}
	if info.Size() < r.maxSize {
		return
	}
	if _, err := r.rotateLocked(); err != nil {
		fmt.Fprintf(r.stderr, "events: rotation: auto-rotate failed: %v\n", err) //nolint:errcheck // best-effort stderr
	}
}

// ForceRotate rotates the active log immediately, ignoring the size
// threshold. Safe to call concurrently with Record. Returns a
// no-op result with Rotated=false if the active log is empty (an
// empty file is never archived).
func (r *FileRecorder) ForceRotate() (RotationResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return RotationResult{}, fmt.Errorf("recorder is closed")
	}
	return r.rotateLocked()
}

// rotateLocked performs the close+rename+open+anchor sequence. It
// must be called with r.mu held. The caller is responsible for
// checking r.closed.
//
// On success, the prior active log is renamed to
// events.jsonl.rotating-<ts> and a background goroutine compresses
// it to its canonical archive basename. The result's Done channel
// closes when that goroutine finishes.
func (r *FileRecorder) rotateLocked() (RotationResult, error) {
	info, err := r.file.Stat()
	if err != nil {
		return RotationResult{}, fmt.Errorf("stat active log: %w", err)
	}
	if info.Size() == 0 {
		return RotationResult{Rotated: false, Reason: "active log is empty"}, nil
	}

	first, last, err := readSeqWindow(r.path)
	if err != nil {
		return RotationResult{}, fmt.Errorf("reading seq window: %w", err)
	}

	ts := time.Now().UTC()
	dir := filepath.Dir(r.path)
	archiveBase := formatArchiveBasename(ts, first, last)
	archivePath := filepath.Join(dir, archiveBase)
	rotatingPath := filepath.Join(dir, formatRotatingBasename(ts, first, last))

	if err := r.file.Close(); err != nil {
		return RotationResult{}, fmt.Errorf("closing active log: %w", err)
	}
	r.file = nil

	if err := os.Rename(r.path, rotatingPath); err != nil {
		// Try to recover: re-open the original path. If that also
		// fails, mark the recorder closed so subsequent Record calls
		// drop cleanly instead of dereferencing a nil file under
		// maybeAutoRotateLocked.
		if newF, openErr := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); openErr == nil {
			r.file = newF
		} else {
			r.closed = true
		}
		return RotationResult{}, fmt.Errorf("renaming active log: %w", err)
	}

	newFile, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return RotationResult{}, fmt.Errorf("opening new active log: %w", err)
	}
	r.file = newFile
	r.recordCount = 0
	r.lastSizeCheck = time.Now()

	payload := RotatedPayload{
		PriorArchive:  archiveBase,
		PriorFirstSeq: first,
		PriorLastSeq:  last,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return RotationResult{}, fmt.Errorf("marshaling anchor payload: %w", err)
	}
	anchor := Event{
		Type:    EventsRotated,
		Actor:   "events",
		Message: fmt.Sprintf("rotated to %s", archiveBase),
		Payload: payloadBytes,
	}
	if err := r.writeRecordLocked(&anchor); err != nil {
		return RotationResult{}, fmt.Errorf("writing anchor event: %w", err)
	}
	if err := r.file.Sync(); err != nil {
		fmt.Fprintf(r.stderr, "events: rotation: sync new active log: %v\n", err) //nolint:errcheck // best-effort stderr
	}

	done := make(chan struct{})
	retainAge := r.archiveRetainAge
	r.rotations.Add(1)
	go func() {
		defer r.rotations.Done()
		defer close(done)
		if err := gzipAndArchive(rotatingPath, archivePath, r.stderr); err != nil {
			// gzipAndArchive already wrote to stderr.
			_ = err
			return
		}
		if err := reapExpiredArchives(dir, retainAge, r.stderr); err != nil {
			fmt.Fprintf(r.stderr, "events: rotation: archive retention: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}()

	return RotationResult{
		Rotated:            true,
		ArchivePath:        archivePath,
		FirstSeq:           first,
		LastSeq:            last,
		AnchorSeq:          anchor.Seq,
		AnchorTimestamp:    anchor.Ts,
		CompressionPending: true,
		Done:               done,
	}, nil
}

// List returns events matching the filter from the underlying file.
func (r *FileRecorder) List(filter Filter) ([]Event, error) {
	return ReadFiltered(r.path, filter)
}

// ListTail returns trailing matching events from the underlying file.
func (r *FileRecorder) ListTail(filter Filter, limit int) ([]Event, error) {
	return ReadFilteredTail(r.path, filter, limit)
}

// LatestSeq returns the highest sequence number in the event log.
func (r *FileRecorder) LatestSeq() (uint64, error) {
	r.mu.Lock()
	seq := r.seq
	r.mu.Unlock()
	return seq, nil
}

// Watch returns a Watcher that yields every retained event with
// Seq > afterSeq exactly once, in seq order, and then tails the active
// log for new events. It is archive-aware in two directions:
//
//   - Resume backfill. When afterSeq is below the live tail, events with
//     Seq > afterSeq may have been rotated out of the active file into a
//     .gz archive (or a rotating-* file mid-gzip). Watch enumerates those
//     windows and the returned watcher replays them, in seq order, before
//     tailing the active file — so an SSE/Last-Event-ID or exporter-cursor
//     reconnect below a rotation boundary no longer loses the gap.
//
//   - Mid-watch rotation. Watch captures a read fd to the current active
//     inode under the recorder lock. Holding that fd keeps the old inode
//     readable to true EOF even after a later rotation renames and gzips
//     it away, so the tail appended between the watcher's last read and
//     the rename is delivered before the watcher switches to the new
//     active file. Pinning the inode also prevents the new active file
//     from reusing its inode number, so rotation stays detectable by
//     inode comparison.
//
// Already-yielded events are deduped via the afterSeq cursor, so an
// overlap between an archive and its transient rotating sibling, or
// between the backfill and the live tail, never double-delivers.
func (r *FileRecorder) Watch(ctx context.Context, afterSeq uint64) (Watcher, error) {
	r.mu.Lock()
	// Capture the active inode's fd, offset, and (via the fd) identity
	// under the lock, so they are all consistent with r.seq and no
	// rotation can interpose between the snapshot and the watcher's first
	// read.
	curr, currInode := openActiveForRead(r.path)
	var offset int64
	if curr != nil && afterSeq >= r.seq {
		// Caller is already current — tail only appends past the live EOF.
		if info, err := curr.Stat(); err == nil {
			offset = info.Size()
		}
	}
	var legs []backfillLeg
	if afterSeq < r.seq {
		// Caller is resuming below the live tail: rotated windows may hold
		// events with Seq > afterSeq that no longer live in the active
		// file. Replay them before tailing from the active file's start.
		legs = collectBackfillLegs(filepath.Dir(r.path), afterSeq)
	}
	r.mu.Unlock()

	var backfill *backfillReader
	if len(legs) > 0 {
		backfill = &backfillReader{dir: filepath.Dir(r.path), legs: legs, stderr: r.stderr}
	}
	return &fileWatcher{
		path:      r.path,
		afterSeq:  afterSeq,
		ctx:       ctx,
		poll:      250 * time.Millisecond,
		stderr:    r.stderr,
		curr:      curr,
		currInode: currInode,
		offset:    offset,
		backfill:  backfill,
		done:      make(chan struct{}),
	}, nil
}

// openActiveForRead opens a read-only fd to the active log and returns it
// with its inode. On any error — including a path momentarily absent
// during a concurrent rotation — it returns (nil, 0); the watcher then
// opens the active file lazily on a later poll.
func openActiveForRead(path string) (*os.File, uint64) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0
	}
	var inode uint64
	if info, err := f.Stat(); err == nil {
		inode = inodeOf(info)
	}
	return f, inode
}

// Close closes the underlying file. It is safe to call multiple times;
// subsequent calls after the first return nil.
//
// Close drains in-flight rotation goroutines before returning so any
// rotating-* sibling files have been promoted to canonical archives
// by the time the caller starts reading. This trade-off — a brief
// block for clean shutdown semantics — matches the architect's
// crash-safe NFR-06 goal: a clean exit must not strand events in a
// rotating-* file that ReadAll wouldn't pick up until the next
// process opens a recorder and runs the orphan reaper.
func (r *FileRecorder) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	file := r.file
	r.file = nil
	r.mu.Unlock()

	r.rotations.Wait()

	if file == nil {
		return nil
	}
	return file.Close()
}

// WaitForRotations blocks until every in-flight rotation goroutine
// has completed. Useful for tests that read archives immediately
// after triggering rotations and for callers that want to confirm
// disk state is fully settled before snapshotting.
func (r *FileRecorder) WaitForRotations() {
	r.rotations.Wait()
}

// errWatcherClosed is returned by Next once the watcher has been closed.
var errWatcherClosed = errors.New("watcher closed")

// fileWatcher yields events with Seq > afterSeq in seq order. It first
// replays any rotated backfill legs (archives + rotating-* files whose
// window sits above afterSeq), then tails the active log via an fd it
// holds open across rotations. Holding the fd keeps the old active inode
// readable to true EOF after a rotation renames it away, so the mid-watch
// tail is not lost; rotation is detected by comparing the path's inode
// against the held fd's inode, and the fd pins the old inode so its number
// cannot be reused by the new active file. The afterSeq cursor dedupes
// against already-yielded events across every seam.
type fileWatcher struct {
	path     string
	afterSeq uint64
	ctx      context.Context
	poll     time.Duration
	stderr   io.Writer // for re-armed backfill diagnostics on multi-rotation

	mu        sync.Mutex // guards backfill, curr, currInode, offset, buf, closed
	backfill  *backfillReader
	curr      *os.File // read fd to the inode currently being tailed
	currInode uint64   // identity of curr (0 = unknown / not yet opened)
	offset    int64    // read position within curr
	buf       []Event  // decoded events awaiting delivery
	closed    bool

	done      chan struct{}
	closeOnce sync.Once
}

// Next blocks until the next event is available, the context is canceled,
// or the watcher is closed.
func (w *fileWatcher) Next() (Event, error) {
	for {
		e, ok, err := w.fill()
		if err != nil {
			return Event{}, err
		}
		if ok {
			return e, nil
		}
		// Nothing available yet — wait for new data, close, or cancel.
		select {
		case <-w.ctx.Done():
			return Event{}, w.ctx.Err()
		case <-w.done:
			return Event{}, errWatcherClosed
		case <-time.After(w.poll):
		}
	}
}

// fill advances the watcher until it can return one event or determines
// that no data is currently available (ok=false, err=nil), in which case
// Next waits before calling again. It runs entirely under w.mu so a
// concurrent Close cannot close the fd mid-read.
func (w *fileWatcher) fill() (Event, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for {
		if w.closed {
			return Event{}, false, errWatcherClosed
		}
		if len(w.buf) > 0 {
			e := w.buf[0]
			w.buf = w.buf[1:]
			return e, true, nil
		}
		// Honor cancellation promptly, even under a continuous event
		// stream where the reads below never yield an empty poll. Buffered
		// events are drained first (above) so a cancel does not discard
		// already-decoded events.
		select {
		case <-w.ctx.Done():
			return Event{}, false, w.ctx.Err()
		default:
		}

		// Phase 1: replay rotated backfill legs before the live tail.
		if w.backfill != nil {
			e, ok, err := w.backfill.next()
			if err != nil {
				return Event{}, false, err
			}
			if ok {
				if e.Seq > w.afterSeq {
					w.afterSeq = e.Seq
					w.buf = append(w.buf, e)
				}
				continue
			}
			w.backfill.close()
			w.backfill = nil
			continue
		}

		// Phase 2: tail the active inode through the held fd.
		if w.curr == nil {
			f, inode := openActiveForRead(w.path)
			if f == nil {
				// Active file momentarily absent (mid-rotation) — wait.
				return Event{}, false, nil
			}
			w.curr = f
			w.currInode = inode
			w.offset = 0
		}

		evts, newOffset, err := readFromOpenFile(w.curr, w.offset)
		if err != nil {
			return Event{}, false, err
		}
		w.offset = newOffset
		buffered := false
		for _, e := range evts {
			if e.Seq > w.afterSeq {
				w.afterSeq = e.Seq
				w.buf = append(w.buf, e)
				buffered = true
			}
		}
		if buffered {
			continue // deliver from buf on the next loop
		}
		if len(evts) > 0 {
			// Read events, but all were at/below afterSeq (already
			// delivered, e.g. re-reading the active file after backfill).
			// Keep scanning forward without waiting.
			continue
		}

		// curr is at EOF. Detect rotation: if the path now points to a
		// different inode, rotateLocked has already closed the old inode's
		// writer (the rename happens after that close), so the tail we
		// hold is final. Drain it once more to capture anything appended
		// between our last read and the rename, then switch to the new
		// active inode.
		if w.currInode != 0 {
			if info, statErr := os.Stat(w.path); statErr == nil {
				if pathInode := inodeOf(info); pathInode != 0 && pathInode != w.currInode {
					tail, tailOffset, err := readFromOpenFile(w.curr, w.offset)
					if err != nil {
						return Event{}, false, err
					}
					w.offset = tailOffset
					for _, e := range tail {
						if e.Seq > w.afterSeq {
							w.afterSeq = e.Seq
							w.buf = append(w.buf, e)
						}
					}
					_ = w.curr.Close()
					w.curr = nil
					w.currInode = 0
					w.offset = 0
					// If more than one rotation elapsed since our last read,
					// the intermediate active inodes we never held have been
					// archived. Re-arm backfill for every window now above
					// the cursor so those are replayed before we tail the
					// current active file. afterSeq (advanced by the drain
					// above) excludes the inode we just finished. A single
					// rotation yields no such windows, so this is a no-op in
					// the common case.
					if legs := collectBackfillLegs(filepath.Dir(w.path), w.afterSeq); len(legs) > 0 {
						w.backfill = &backfillReader{dir: filepath.Dir(w.path), legs: legs, stderr: w.stderr}
					}
					continue // drain any re-armed backfill, then reopen active
				}
			}
		}

		// No new data and no rotation — ask Next to wait.
		return Event{}, false, nil
	}
}

// Close stops the watcher, unblocks any pending Next call, and releases
// the held fd and any open backfill leg. Safe to call concurrently with
// Next and safe to call multiple times.
func (w *fileWatcher) Close() error {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		if w.curr != nil {
			_ = w.curr.Close()
			w.curr = nil
		}
		if w.backfill != nil {
			w.backfill.close()
			w.backfill = nil
		}
		w.mu.Unlock()
		close(w.done)
	})
	return nil
}
