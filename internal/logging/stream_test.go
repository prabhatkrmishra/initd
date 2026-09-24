package logging

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func seedStreamFiles(t *testing.T, dir, boot string, n int) []StoredEntry {
	t.Helper()
	w, err := NewFileWriter(dir, boot, "h", 1024)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		unit := "a.service"
		if i%3 == 0 {
			unit = "b.service"
		}
		if err := w.Append(StoredEntry{Unit: unit, PID: 1, Priority: 6, Message: fmt.Sprintf("line-%04d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := ReadAll(ListFiles(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n {
		t.Fatalf("seeded %d, read %d", n, len(entries))
	}
	return entries
}

// Paged head-after-cursor reads must reconstruct exactly what the
// materializing query returns for the same filter.
func TestStreamMatchesQuery(t *testing.T) {
	dir := t.TempDir()
	all := seedStreamFiles(t, dir, "boot-s", 120)
	files := ListFiles(dir)
	if len(files) < 2 {
		t.Fatalf("want rotation across files, got %d", len(files))
	}
	f := JournalFilter{Units: []string{"a.service"}, Lines: 25, LinesPlus: true}
	full := QueryJournal(all, JournalFilter{Units: []string{"a.service"}})
	var got []StoredEntry
	cursor := ""
	after := false
	for {
		f.Cursor, f.CursorAfter = cursor, after
		page, err := QueryJournalStream(files, f)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		got = append(got, page...)
		cursor, after = page[len(page)-1].Cursor, true
		if len(page) < 25 {
			break
		}
	}
	if len(got) != len(full) {
		t.Fatalf("paged %d != full %d", len(got), len(full))
	}
	for i := range got {
		if got[i].Cursor != full[i].Cursor || got[i].Message != full[i].Message {
			t.Fatalf("entry %d differs: %+v vs %+v", i, got[i], full[i])
		}
	}
}

func TestStreamUnknownCursor(t *testing.T) {
	dir := t.TempDir()
	seedStreamFiles(t, dir, "boot-s", 10)
	out, err := QueryJournalStream(ListFiles(dir), JournalFilter{Lines: 5, LinesPlus: true, Cursor: "nope", CursorAfter: true})
	if err != nil {
		t.Fatal(err)
	}
	if out != nil && len(out) != 0 {
		t.Fatalf("unknown cursor should yield nothing, got %d", len(out))
	}
}

func TestStreamRespectsGrepAndLimit(t *testing.T) {
	dir := t.TempDir()
	seedStreamFiles(t, dir, "boot-s", 50)
	out, err := QueryJournalStream(ListFiles(dir), JournalFilter{Grep: "line-004", Lines: 5, LinesPlus: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("expected grep hits")
	}
	for _, e := range out {
		if e.Message != "line-0004" && e.Message != "line-0040" && e.Message != "line-0041" && e.Message != "line-0042" && e.Message != "line-0043" && e.Message != "line-0044" && e.Message != "line-0045" && e.Message != "line-0046" && e.Message != "line-0047" && e.Message != "line-0048" && e.Message != "line-0049" {
			// line-004x all contain "line-004"; line-0004 too. Anything else is wrong.
			if len(e.Message) < 8 || e.Message[:8] != "line-004" && e.Message != "line-0004" {
				t.Fatalf("grep mismatch: %q", e.Message)
			}
		}
	}
	if len(out) > 5 {
		t.Fatalf("limit ignored: %d", len(out))
	}
}

// Tail-N reads must match the materializing tail without loading everything.
func TestTailMatchesQuery(t *testing.T) {
	dir := t.TempDir()
	all := seedStreamFiles(t, dir, "boot-s", 120)
	files := ListFiles(dir)
	for _, n := range []int{1, 10, 50} {
		f := JournalFilter{Units: []string{"b.service"}, Lines: n}
		full := QueryJournal(all, f)
		got, err := QueryJournalTail(files, f)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(full) {
			t.Fatalf("n=%d: tail %d != full %d", n, len(got), len(full))
		}
		for i := range got {
			if got[i].Cursor != full[i].Cursor {
				t.Fatalf("n=%d entry %d differs", n, i)
			}
		}
	}
}

// Files older than the window must be skipped without opening.
func TestFilesNewerThan(t *testing.T) {
	dir := t.TempDir()
	old, recent := dir+"/old.jsonl", dir+"/recent.jsonl"
	if err := os.WriteFile(old, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recent, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMicro()
	kept := filesNewerThan([]string{old, recent, dir + "/missing.jsonl"}, now-10*60*1000000)
	if len(kept) != 2 || kept[0] != recent || kept[1] != dir+"/missing.jsonl" {
		t.Fatalf("kept = %v", kept)
	}
	if got := filesNewerThan([]string{old, recent}, 0); len(got) != 2 {
		t.Fatalf("zero since must keep all: %v", got)
	}
}

// A trailing window over mostly-stale files returns only fresh matches.
func TestStreamSkipsStaleFiles(t *testing.T) {
	dir := t.TempDir()
	w, err := NewFileWriter(dir, "boot-old", "h", 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	oldTs := time.Now().Add(-2 * time.Hour).UnixMicro()
	for i := 0; i < 20; i++ {
		_ = w.Append(StoredEntry{Unit: "old.service", PID: 1, Priority: 6, Message: "ancient", RealtimeUsec: oldTs})
	}
	_ = w.Sync()
	_ = w.Close()
	for _, f := range ListFiles(dir) {
		past := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(f, past, past); err != nil {
			t.Fatal(err)
		}
	}
	w2, err := NewFileWriter(dir, "boot-new", "h", 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		_ = w2.Append(StoredEntry{Unit: "new.service", PID: 1, Priority: 6, Message: "fresh"})
	}
	_ = w2.Sync()
	_ = w2.Close()
	since := time.Now().Add(-10 * time.Minute).UnixMicro()
	out, err := QueryJournalStream(ListFiles(dir), JournalFilter{SinceUsec: since, Lines: 100, LinesPlus: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 5 {
		t.Fatalf("got %d, want only the 5 fresh entries", len(out))
	}
	for _, e := range out {
		if e.Unit != "new.service" {
			t.Fatalf("stale entry leaked: %+v", e)
		}
	}
}
