package events

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// backfillLeg identifies one rotated seq window that a resuming watcher
// must replay before it tails the live active log. The concrete file is
// resolved lazily at read time — the canonical .gz archive first, the
// in-flight rotating-* sibling as a fallback — so a window that is
// gzipped-and-renamed between watcher construction and the read is still
// found on whichever side of the rename it currently sits.
type backfillLeg struct {
	ts    time.Time
	first uint64
	last  uint64
}

// collectBackfillLegs enumerates the rotated seq windows in dir that can
// still contain events with Seq > afterSeq: canonical .gz archives plus
// any events.jsonl.rotating-* files left mid-gzip by a recent rotation.
// The returned legs are sorted by FirstSeq ascending (chronological) and
// de-duplicated by seq window, so a window present as both an archive and
// its not-yet-removed rotating sibling collapses to a single leg. Windows
// whose LastSeq is at or below afterSeq are dropped — the caller has
// already seen them.
//
// A directory-listing failure returns nil: the watcher then simply tails
// the active file, and its afterSeq cursor still guarantees correct,
// duplicate-free delivery of everything that lives there.
func collectBackfillLegs(dir string, afterSeq uint64) []backfillLeg {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	// A rotation is uniquely identified by its (first,last) seq window, so
	// an archive and its transient rotating sibling map to the same key.
	byWindow := make(map[[2]uint64]backfillLeg)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if info, err := parseArchiveBasename(name); err == nil {
			key := [2]uint64{info.FirstSeq, info.LastSeq}
			if _, ok := byWindow[key]; !ok {
				byWindow[key] = backfillLeg{ts: info.Timestamp, first: info.FirstSeq, last: info.LastSeq}
			}
			continue
		}
		if ts, first, last, ok := parseRotatingBasename(name); ok {
			key := [2]uint64{first, last}
			if _, ok := byWindow[key]; !ok {
				byWindow[key] = backfillLeg{ts: ts, first: first, last: last}
			}
		}
	}

	legs := make([]backfillLeg, 0, len(byWindow))
	for _, leg := range byWindow {
		if leg.last <= afterSeq {
			continue
		}
		legs = append(legs, leg)
	}
	sort.Slice(legs, func(i, j int) bool { return legs[i].first < legs[j].first })
	return legs
}

// backfillReader streams the events of a sequence of backfillLegs in seq
// order. Legs are opened lazily, one at a time, so memory stays bounded to
// a single archive's decode buffers even when a resume spans many large
// archives. Malformed lines (partial writes) are skipped, matching the
// live readers.
type backfillReader struct {
	dir    string
	legs   []backfillLeg
	idx    int
	stderr io.Writer

	f       *os.File
	gr      *gzip.Reader
	scanner *bufio.Scanner
}

// next returns the next event across all remaining legs. The bool is false
// with a nil error once every leg is exhausted. A leg that vanishes
// between enumeration and read (a rotating file gzipped away, an archive
// pruned by retention) is skipped with a stderr note rather than failing
// the whole stream — the watcher's afterSeq cursor and its live tail keep
// delivery correct across such a gap.
func (b *backfillReader) next() (Event, bool, error) {
	for {
		if b.scanner == nil {
			if b.idx >= len(b.legs) {
				return Event{}, false, nil
			}
			leg := b.legs[b.idx]
			b.idx++
			if err := b.openLeg(leg); err != nil {
				fmt.Fprintf(b.stderr, "events: watch backfill: seq %d-%d: %v\n", leg.first, leg.last, err) //nolint:errcheck // best-effort stderr
				continue
			}
		}
		if b.scanner.Scan() {
			var e Event
			if err := json.Unmarshal(b.scanner.Bytes(), &e); err != nil {
				continue // skip malformed lines
			}
			return e, true, nil
		}
		if err := b.scanner.Err(); err != nil {
			b.closeLeg()
			return Event{}, false, fmt.Errorf("scanning backfill leg: %w", err)
		}
		b.closeLeg()
	}
}

// openLeg resolves leg to a concrete file and opens a line scanner over
// it. It prefers the canonical .gz archive and falls back to the in-flight
// rotating-* sibling, so the read succeeds whichever side of the
// gzip+rename the window occupies when we reach it.
func (b *backfillReader) openLeg(leg backfillLeg) error {
	archivePath := filepath.Join(b.dir, formatArchiveBasename(leg.ts, leg.first, leg.last))
	if f, err := os.Open(archivePath); err == nil {
		gr, err := gzip.NewReader(f)
		if err != nil {
			_ = f.Close()
			return fmt.Errorf("gunzip %q: %w", filepath.Base(archivePath), err)
		}
		b.f = f
		b.gr = gr
		b.scanner = newEventLineScanner(gr)
		return nil
	}

	rotatingPath := filepath.Join(b.dir, formatRotatingBasename(leg.ts, leg.first, leg.last))
	if f, err := os.Open(rotatingPath); err == nil {
		b.f = f
		b.scanner = newEventLineScanner(f)
		return nil
	}

	return fmt.Errorf("neither archive nor rotating file present")
}

// closeLeg releases the current leg's open handles and clears the scanner
// so next() advances to the following leg.
func (b *backfillReader) closeLeg() {
	if b.gr != nil {
		_ = b.gr.Close()
		b.gr = nil
	}
	if b.f != nil {
		_ = b.f.Close()
		b.f = nil
	}
	b.scanner = nil
}

// close releases any open leg and marks the reader exhausted. Safe to call
// multiple times.
func (b *backfillReader) close() {
	b.closeLeg()
	b.idx = len(b.legs)
}

// newEventLineScanner returns a bufio.Scanner tuned for JSONL event lines,
// matching the 1 MiB line cap used by the batch readers.
func newEventLineScanner(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return s
}
