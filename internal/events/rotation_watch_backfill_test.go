package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// drainWatcherN pulls exactly want events from w or fails after the
// watcher's context deadline elapses.
func drainWatcherN(t *testing.T, w Watcher, want int) []Event {
	t.Helper()
	got := make([]Event, 0, want)
	for i := 0; i < want; i++ {
		e, err := w.Next()
		if err != nil {
			t.Fatalf("Next %d/%d: %v", i, want, err)
		}
		got = append(got, e)
	}
	return got
}

// writeJSONL stages a plain JSONL events file at path — used to construct
// archive/rotating fixtures without driving a live recorder.
func writeJSONL(t *testing.T, path string, evts []Event) {
	t.Helper()
	var buf bytes.Buffer
	for _, e := range evts {
		data, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal fixture event: %v", err)
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("writing fixture %q: %v", path, err)
	}
}

// TestWatchResumeBackfillsAcrossRotation exercises acceptance (a): a
// watcher resumed with afterSeq below a rotation boundary must stream the
// archived gap (events that were rotated into a .gz archive) before it
// tails the active file.
func TestWatchResumeBackfillsAcrossRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	// seq 1..4 land in the active file, then rotate them into an archive.
	for i := 1; i <= 4; i++ {
		rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: fmt.Sprintf("pre-%d", i)})
	}
	res, err := rec.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	if res.Done != nil {
		<-res.Done // anchor written as seq 5
	}
	// seq 6..8 land in the new active file.
	for i := 6; i <= 8; i++ {
		rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: fmt.Sprintf("post-%d", i)})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Resume from seq 2 — below the rotation boundary. seq 3,4 live only in
	// the .gz archive now; a non-archive-aware Watch would skip them.
	w, err := rec.Watch(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck // test cleanup

	got := drainWatcherN(t, w, 6) // expect seq 3,4,5,6,7,8
	wantSeqs := []uint64{3, 4, 5, 6, 7, 8}
	for i, e := range got {
		if e.Seq != wantSeqs[i] {
			t.Errorf("event %d Seq = %d, want %d", i, e.Seq, wantSeqs[i])
		}
	}
	if got[2].Type != EventsRotated {
		t.Errorf("event at seq 5 Type = %q, want %q (rotation anchor)", got[2].Type, EventsRotated)
	}
	if got[0].Subject != "pre-3" || got[1].Subject != "pre-4" {
		t.Errorf("archived backfill subjects = [%q,%q], want [pre-3,pre-4]", got[0].Subject, got[1].Subject)
	}
}

// TestWatchDeliversPendingTailBeforeRotation exercises acceptance (b): a
// mid-watch rotation must not strand events appended to the old active
// file between the watcher's last read and the rename.
func TestWatchDeliversPendingTailBeforeRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Start watching from the very beginning with no events yet.
	w, err := rec.Watch(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck // test cleanup

	// Append two events to the active inode but do NOT drain them: this is
	// the pending tail the watcher has not read when rotation strikes.
	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "tail-1"}) // seq 1
	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "tail-2"}) // seq 2

	// Rotate: the old active inode is renamed+gzipped; a fresh inode gets
	// the anchor. The pending tail now lives only on the old inode.
	res, err := rec.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	if res.Done != nil {
		<-res.Done // anchor written as seq 3
	}
	rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "post-1"}) // seq 4

	// The pending tail (seq 1,2) must arrive before the anchor(3) and post(4).
	got := drainWatcherN(t, w, 4)
	wantSeqs := []uint64{1, 2, 3, 4}
	for i, e := range got {
		if e.Seq != wantSeqs[i] {
			t.Errorf("event %d Seq = %d, want %d (pending tail lost on rotation)", i, e.Seq, wantSeqs[i])
		}
	}
	if got[0].Subject != "tail-1" || got[1].Subject != "tail-2" {
		t.Errorf("pending tail subjects = [%q,%q], want [tail-1,tail-2]", got[0].Subject, got[1].Subject)
	}
}

// TestWatchNoDuplicateAcrossArchiveActiveSeam exercises acceptance (c): a
// resume that spans multiple archives and the active file must deliver
// every retained event exactly once, in seq order, with no duplication at
// the archive→active seam.
func TestWatchNoDuplicateAcrossArchiveActiveSeam(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	// Two rotations produce two archives; then a few live events remain.
	for i := 0; i < 3; i++ {
		rec.Record(Event{Type: BeadCreated, Actor: "a"}) // seq 1,2,3
	}
	res1, err := rec.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate 1: %v", err)
	}
	if res1.Done != nil {
		<-res1.Done // anchor seq 4
	}
	for i := 0; i < 3; i++ {
		rec.Record(Event{Type: BeadCreated, Actor: "b"}) // seq 5,6,7
	}
	res2, err := rec.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate 2: %v", err)
	}
	if res2.Done != nil {
		<-res2.Done // anchor seq 8
	}
	for i := 0; i < 2; i++ {
		rec.Record(Event{Type: BeadClosed, Actor: "c"}) // seq 9,10
	}

	latest, err := rec.LatestSeq()
	if err != nil {
		t.Fatal(err)
	}
	if latest != 10 {
		t.Fatalf("LatestSeq = %d, want 10", latest)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w, err := rec.Watch(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck // test cleanup

	got := drainWatcherN(t, w, int(latest))
	seen := map[uint64]int{}
	for _, e := range got {
		seen[e.Seq]++
	}
	for s := uint64(1); s <= latest; s++ {
		if seen[s] != 1 {
			t.Errorf("seq %d delivered %d times, want exactly 1", s, seen[s])
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Errorf("delivery out of order at %d: seq %d after seq %d", i, got[i].Seq, got[i-1].Seq)
		}
	}
}

// TestWatchSurvivesTwoRotationsBetweenPolls guards the multi-rotation
// path: when more than one rotation elapses between a watcher's reads, the
// active inode it held and every intermediate inode it never held have all
// been archived. The watcher must still deliver each archived event
// exactly once, in seq order, before tailing the current active file.
func TestWatchSurvivesTwoRotationsBetweenPolls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Tail-only watch established while the first inode is active.
	w, err := rec.Watch(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck // test cleanup

	// Write to inode #1 but do not drain — the watcher still holds inode #1.
	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "a-1"}) // seq 1
	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "a-2"}) // seq 2

	// First rotation: inode #1 archived, inode #2 gets anchor(3).
	res1, err := rec.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate 1: %v", err)
	}
	if res1.Done != nil {
		<-res1.Done
	}
	rec.Record(Event{Type: BeadUpdated, Actor: "human", Subject: "b-1"}) // seq 4

	// Second rotation before the watcher ever tailed inode #2: inode #2
	// archived, inode #3 gets anchor(5). The watcher never held inode #2.
	res2, err := rec.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate 2: %v", err)
	}
	if res2.Done != nil {
		<-res2.Done
	}
	rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "c-1"}) // seq 6

	// Every event must arrive, in order: inode#1 tail (1,2), inode#2's
	// archived anchor(3)+b-1(4), then the live inode#3 anchor(5)+c-1(6).
	got := drainWatcherN(t, w, 6)
	wantSeqs := []uint64{1, 2, 3, 4, 5, 6}
	for i, e := range got {
		if e.Seq != wantSeqs[i] {
			t.Errorf("event %d Seq = %d, want %d (multi-rotation gap)", i, e.Seq, wantSeqs[i])
		}
	}
	if got[0].Subject != "a-1" || got[3].Subject != "b-1" || got[5].Subject != "c-1" {
		t.Errorf("subjects = [%q..%q..%q], want a-1..b-1..c-1", got[0].Subject, got[3].Subject, got[5].Subject)
	}
}

// TestBackfillReaderPrefersArchiveFallsBackToRotating verifies the leg
// resolver reads the canonical .gz archive when present and falls back to
// an in-flight rotating-* sibling when the archive has not yet appeared —
// the race window between ForceRotate and gzip completion.
func TestBackfillReaderPrefersArchiveFallsBackToRotating(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	// Stage a rotating-* file (no archive yet) covering seq 1-2.
	rotating := filepath.Join(dir, formatRotatingBasename(ts, 1, 2))
	writeJSONL(t, rotating, []Event{
		{Seq: 1, Type: BeadCreated, Actor: "x"},
		{Seq: 2, Type: BeadCreated, Actor: "x"},
	})

	legs := []backfillLeg{{ts: ts, first: 1, last: 2}}
	br := &backfillReader{dir: dir, legs: legs, stderr: io.Discard}
	defer br.close()

	got := drainBackfill(t, br)
	if len(got) != 2 || got[0].Seq != 1 || got[1].Seq != 2 {
		t.Fatalf("rotating fallback: got %+v, want seq [1,2]", got)
	}
}

// TestCollectBackfillLegsFiltersAndDedups verifies leg collection skips
// windows already fully below afterSeq and collapses an archive and its
// not-yet-removed rotating sibling into a single leg.
func TestCollectBackfillLegsFiltersAndDedups(t *testing.T) {
	dir := t.TempDir()
	ts1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ts2 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	// Window seq 1-3 present as BOTH an archive and a rotating sibling.
	writeJSONL(t, filepath.Join(dir, formatRotatingBasename(ts1, 1, 3)), []Event{{Seq: 1}})
	stageArchive(t, dir, ts1, 1, 3, []Event{{Seq: 1}, {Seq: 2}, {Seq: 3}})
	// Window seq 4-6 present only as an archive.
	stageArchive(t, dir, ts2, 4, 6, []Event{{Seq: 4}, {Seq: 5}, {Seq: 6}})

	// afterSeq=3 excludes the 1-3 window entirely and keeps 4-6.
	legs := collectBackfillLegs(dir, 3)
	if len(legs) != 1 {
		t.Fatalf("legs = %+v, want exactly the 4-6 window", legs)
	}
	if legs[0].first != 4 || legs[0].last != 6 {
		t.Errorf("leg = %d-%d, want 4-6", legs[0].first, legs[0].last)
	}

	// afterSeq=0 keeps both windows, de-duplicated to one leg each.
	legs = collectBackfillLegs(dir, 0)
	if len(legs) != 2 {
		t.Fatalf("legs = %+v, want two de-duplicated windows", legs)
	}
	if legs[0].first != 1 || legs[1].first != 4 {
		t.Errorf("legs order = [%d,%d], want first-seq ascending [1,4]", legs[0].first, legs[1].first)
	}
}

// drainBackfill pulls every event from a backfillReader.
func drainBackfill(t *testing.T, br *backfillReader) []Event {
	t.Helper()
	var got []Event
	for {
		e, ok, err := br.next()
		if err != nil {
			t.Fatalf("backfill next: %v", err)
		}
		if !ok {
			return got
		}
		got = append(got, e)
	}
}

// stageArchive writes a gzip archive fixture in dir using the canonical
// basename for (ts, first, last).
func stageArchive(t *testing.T, dir string, ts time.Time, first, last uint64, evts []Event) {
	t.Helper()
	plain := filepath.Join(dir, "stage-tmp.jsonl")
	writeJSONL(t, plain, evts)
	dest := filepath.Join(dir, formatArchiveBasename(ts, first, last))
	if err := gzipAndArchive(plain, dest, io.Discard); err != nil {
		t.Fatalf("staging archive: %v", err)
	}
}
