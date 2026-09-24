package logging

import (
	"testing"
)

func TestFileWriterSeqAcrossRotate(t *testing.T) {
	dir := t.TempDir()
	w, err := NewFileWriter(dir, "boot-seq", "h", 400)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if err := w.Append(StoredEntry{Unit: "a.service", Message: "padding line to force multiple rotates over tiny cap xx"}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if n := len(ListFiles(dir)); n < 3 {
		t.Fatalf("files = %d, want >=3 rotates", n)
	}
	entries, _ := ReadAll(ListFiles(dir))
	if len(entries) != 30 {
		t.Fatalf("entries = %d, want 30", len(entries))
	}
	seen := map[uint64]bool{}
	seenCur := map[string]bool{}
	for i, e := range entries {
		if e.Seq == 0 {
			t.Fatalf("entry %d has zero seq", i)
		}
		if seen[e.Seq] {
			t.Fatalf("duplicate seq %d", e.Seq)
		}
		seen[e.Seq] = true
		if seenCur[e.Cursor] {
			t.Fatalf("duplicate cursor %q", e.Cursor)
		}
		seenCur[e.Cursor] = true
		if i > 0 && entries[i-1].Seq >= e.Seq {
			// ReadAll sorts by Seq;monotonic file seq must already be ordered.
			t.Fatalf("seq not monotonic at %d: %d >= %d", i, entries[i-1].Seq, e.Seq)
		}
	}
	// Reopen must continue, not reset.
	w2, err := NewFileWriter(dir, "boot-seq", "h", 400)
	if err != nil {
		t.Fatal(err)
	}
	_ = w2.Append(StoredEntry{Unit: "a.service", Message: "after-reopen"})
	_ = w2.Close()
	entries2, _ := ReadAll(ListFiles(dir))
	if len(entries2) != 31 {
		t.Fatalf("entries2 = %d, want 31", len(entries2))
	}
	if entries2[30].Seq <= entries[29].Seq {
		t.Fatalf("reopen seq %d did not advance past %d", entries2[30].Seq, entries[29].Seq)
	}
}
