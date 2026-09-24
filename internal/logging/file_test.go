package logging

import (
	"os"
	"path/filepath"
	"testing"
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
