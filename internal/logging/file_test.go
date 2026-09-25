package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileWriterAppendRead(t *testing.T) {
	dir := t.TempDir()
	w, err := NewFileWriter(dir, "boot-1", "testhost", 0)
	if err != nil {
		t.Fatalf("NewFileWriter: %v", err)
	}
	for _, msg := range []string{"first", "second", "third"} {
		if err := w.Append(StoredEntry{Unit: "a.service", PID: 1, Priority: 6, Message: msg}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	entries, err := ReadAll(ListFiles(dir))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	if entries[0].Message != "first" || entries[2].Message != "third" {
		t.Fatalf("order = %q %q %q", entries[0].Message, entries[1].Message, entries[2].Message)
	}
	if entries[0].Seq == 0 || entries[0].Cursor == "" || entries[0].BootID != "boot-1" || entries[0].Hostname != "testhost" {
		t.Fatalf("metadata missing: %+v", entries[0])
	}
}

func TestFileWriterSeqContinuity(t *testing.T) {
	dir := t.TempDir()
	w, err := NewFileWriter(dir, "boot-1", "h", 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Append(StoredEntry{Message: "one"})
	_ = w.Append(StoredEntry{Message: "two"})
	_ = w.Close()
	w2, err := NewFileWriter(dir, "boot-1", "h", 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = w2.Append(StoredEntry{Message: "three"})
	_ = w2.Close()
	entries, _ := ReadAll(ListFiles(dir))
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	if !(entries[0].Seq < entries[1].Seq && entries[1].Seq < entries[2].Seq) {
		t.Fatalf("seqs not ordered: %d %d %d", entries[0].Seq, entries[1].Seq, entries[2].Seq)
	}
}

func TestFileWriterRotation(t *testing.T) {
	dir := t.TempDir()
	w, err := NewFileWriter(dir, "boot-1", "h", 200)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		_ = w.Append(StoredEntry{Unit: "a.service", Message: "padding line to force rotation over the tiny cap"})
	}
	_ = w.Close()
	if n := len(ListFiles(dir)); n < 2 {
		t.Fatalf("files = %d, want rotation to create >= 2", n)
	}
	entries, _ := ReadAll(ListFiles(dir))
	if len(entries) != 20 {
		t.Fatalf("entries = %d, want 20", len(entries))
	}
}

func TestReadAllSkipsTornLines(t *testing.T) {
	dir := t.TempDir()
	raw := "{\"MESSAGE\":\"good\",\"seq\":1}\n{\"MESSAGE\":\"torn\",\n{\"MESSAGE\":\"alsogood\",\"seq\":2}\n"
	if err := os.WriteFile(filepath.Join(dir, "b.jsonl"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, _ := ReadAll(ListFiles(dir))
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (torn skipped)", len(entries))
	}
}

func TestBufferAttachesFile(t *testing.T) {
	dir := t.TempDir()
	w, err := NewFileWriter(dir, "boot-1", "h", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	b := NewBuffer(10)
	b.AttachFile(w)
	b.Add(Entry{Unit: "a.service", PID: 1, Level: LevelInfo, Message: "hello"})
	if len(b.Entries()) != 1 {
		t.Fatal("ring should keep the line")
	}
	_ = w.Sync()
	_ = w.Close()
	entries, _ := ReadAll(ListFiles(dir))
	if len(entries) != 1 || entries[0].Message != "hello" || entries[0].Priority != 6 {
		t.Fatalf("file copy = %+v", entries)
	}
}

func TestEntryHelpers(t *testing.T) {
	e := Entry{Unit: "foo.service", Level: LevelError}
	if e.Priority() != 3 || e.Identifier() != "foo" {
		t.Fatalf("helpers = %d/%q", e.Priority(), e.Identifier())
	}
	e = Entry{Unit: "bar", Level: LevelInfo}
	if e.Priority() != 6 || e.Identifier() != "bar" {
		t.Fatalf("helpers = %d/%q", e.Priority(), e.Identifier())
	}
}

// Retention enforced on rotate must bound file count and bytes while never
// deleting the active file.
func TestRetentionOnRotate(t *testing.T) {
	dir := t.TempDir()
	w, err := NewFileWriter(dir, "boot-r", "h", 200)
	if err != nil {
		t.Fatal(err)
	}
	w.SetRetention(3, 1<<20)
	for i := 0; i < 30; i++ {
		_ = w.Append(StoredEntry{Unit: "a.service", Message: "padding line to force rotation over the tiny cap xx"})
	}
	_ = w.Sync()
	files := ListFiles(dir)
	if len(files) > 4 { // 3 retained + active
		t.Fatalf("files = %d, want <= 4", len(files))
	}
	active := dir + "/boot-r.jsonl"
	found := false
	for _, f := range files {
		if f == active {
			found = true
		}
	}
	if !found {
		t.Fatalf("active file must survive retention: %v", files)
	}
	entries, _ := ReadAll(files)
	if len(entries) == 0 {
		t.Fatalf("retention must leave readable history")
	}
	_ = w.Close()
}

// A rotation hands over a fresh buffer, so the first line written into it has
// to reach disk on its own rather than wait for a later write to flush it.
func TestFirstLineAfterRotateReachesDisk(t *testing.T) {
	dir := t.TempDir()
	w, err := NewFileWriter(dir, "boot-f", "h", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := w.Append(StoredEntry{Unit: "a.service", Message: "before the rotate"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Rotate(); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(StoredEntry{Unit: "a.service", Message: "after the rotate"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "boot-f.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "after the rotate") {
		t.Fatalf("the first line of the new generation stayed in the buffer: %q", raw)
	}
}

// Retention walks generations oldest-first. Boot IDs are random UUIDs, so
// ordering by name instead of mtime would drop this boot's newest history and
// keep a generation from a year ago.
func TestRetentionTrimsOldestByTimeNotName(t *testing.T) {
	dir := t.TempDir()
	ancient := time.Now().AddDate(-2, 0, 0)
	aged := filepath.Join(dir, "zzzzzzzz.jsonl") // sorts after this boot's name
	if err := os.WriteFile(aged, []byte("{\"MESSAGE\":\"from last year\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(aged, ancient, ancient); err != nil {
		t.Fatal(err)
	}

	w, err := NewFileWriter(dir, "aaaa-boot", "h", 200)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.SetRetention(1, 0)
	for i := 0; i < 8; i++ {
		_ = w.Append(StoredEntry{Unit: "a.service", Message: "padding line to force rotation over the tiny cap xx"})
	}
	_ = w.Sync()

	files := ListFiles(dir)
	for _, f := range files {
		if f == aged {
			t.Fatalf("retention kept an ancient generation and deleted recent ones: %v", files)
		}
	}
	var closed []string
	for _, f := range files {
		if filepath.Base(f) != filepath.Base(w.ActivePath()) {
			closed = append(closed, f)
		}
	}
	if len(files) != 2 || len(closed) != 1 {
		t.Fatalf("files = %v, want one retained generation plus the active file", files)
	}
	if entries, _ := ReadAll(files); len(entries) == 0 {
		t.Fatal("retention left no readable history")
	}
}

// The writer pins its own file with the freshest mtime, so age expiry can only
// reach history once that file has been closed into a generation.
func TestRotateIfStaleClosesAgedHistory(t *testing.T) {
	dir := t.TempDir()
	w, err := NewFileWriter(dir, "boot-s", "h", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := w.Append(StoredEntry{Unit: "a.service", Message: "old news"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().AddDate(0, 0, -20)
	if err := os.Chtimes(w.ActivePath(), stale, stale); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().AddDate(0, 0, -10)
	rotated, err := w.RotateIfStale(cutoff)
	if err != nil {
		t.Fatalf("RotateIfStale: %v", err)
	}
	if !rotated {
		t.Fatal("a file older than the cutoff should close into a generation")
	}
	if _, err := os.Stat(w.ActivePath()); err != nil {
		t.Fatalf("the writer should have a fresh active file: %v", err)
	}
	entries, _ := ReadAll(ListFiles(dir))
	if len(entries) != 1 || entries[0].Message != "old news" {
		t.Fatalf("aged entry not preserved in the closed generation: %+v", entries)
	}

	// Still empty, so nothing to expire and no second generation to add.
	if rotated, err := w.RotateIfStale(cutoff); rotated || err != nil {
		t.Fatalf("an empty active file should not rotate: %v %v", rotated, err)
	}

	// Fresh data must not be dragged out by a stale-looking directory.
	if err := w.Append(StoredEntry{Unit: "a.service", Message: "new news"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if rotated, err := w.RotateIfStale(cutoff); rotated || err != nil {
		t.Fatalf("a freshly written active file should stay open: %v %v", rotated, err)
	}
	if n := len(ListFiles(dir)); n != 2 {
		t.Fatalf("files = %d, want the aged generation plus the active file", n)
	}
}

// TestListFilesOrdersByTimeNotName guards the tail query: files are named after
// a random boot UUID, so name order is unrelated to chronology. A reader that
// walks them "oldest first" by name starts at an arbitrary boot and hands
// `journalctl -n 20` stale entries.
func TestListFilesOrdersByTimeNotName(t *testing.T) {
	dir := t.TempDir()
	// Lexically "0000..." is the newest file on disk; "ffff..." the oldest.
	write := func(name string, mod time.Time) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
	}
	base := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	write("ffff1111.jsonl", base)
	write("0000aaaa-100.jsonl", base.Add(time.Hour))
	write("0000aaaa.jsonl", base.Add(2*time.Hour))
	write("ignored.txt", base.Add(3*time.Hour))

	got := ListFiles(dir)
	want := []string{"ffff1111.jsonl", "0000aaaa-100.jsonl", "0000aaaa.jsonl"}
	if len(got) != len(want) {
		t.Fatalf("ListFiles returned %d files, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if filepath.Base(got[i]) != want[i] {
			t.Errorf("position %d = %s, want %s", i, filepath.Base(got[i]), want[i])
		}
	}
}
