package logging

import "testing"

func TestQueryExcludeAndInvocation(t *testing.T) {
	entries := []StoredEntry{
		{Seq: 1, Cursor: "c1", RealtimeUsec: 1000, Unit: "a.service", Identifier: "keep", InvocationID: "r1", Message: "one"},
		{Seq: 2, Cursor: "c2", RealtimeUsec: 2000, Unit: "a.service", Identifier: "drop", InvocationID: "r1", Message: "two"},
		{Seq: 3, Cursor: "c3", RealtimeUsec: 3000, Unit: "a.service", Identifier: "keep", InvocationID: "r2", Message: "three"},
	}
	out := QueryJournal(entries, JournalFilter{ExcludeIdentifier: "DROP"})
	if len(out) != 2 {
		t.Fatalf("exclude = %d, want 2", len(out))
	}
	out = QueryJournal(entries, JournalFilter{Invocation: "r2"})
	if len(out) != 1 || out[0].Message != "three" {
		t.Fatalf("invocation = %+v", out)
	}
	out = QueryJournal(entries, JournalFilter{Units: []string{"a.service"}, LatestInvocation: true})
	if len(out) != 1 || out[0].Message != "three" {
		t.Fatalf("latest = %+v", out)
	}
	// Legacy entries without ids pass through instead of vanishing.
	legacy := []StoredEntry{
		{Seq: 1, Cursor: "c1", RealtimeUsec: 1000, Unit: "a.service", Message: "old"},
	}
	out = QueryJournal(legacy, JournalFilter{Units: []string{"a.service"}, LatestInvocation: true})
	if len(out) != 1 {
		t.Fatalf("legacy latest = %+v", out)
	}
	if invs := Invocations(entries); len(invs) != 2 || invs[0].ID != "r1" || invs[1].Count != 1 {
		t.Fatalf("invocations = %+v", invs)
	}
}

func TestBufferStampsInvocation(t *testing.T) {
	dir := t.TempDir()
	w, err := NewFileWriter(dir, "boot-1", "testhost", 0)
	if err != nil {
		t.Fatalf("NewFileWriter: %v", err)
	}
	b := NewBuffer(16)
	b.AttachFile(w)
	b.SetInvocation("run-1")
	b.Add(Entry{Unit: "a.service", Message: "first"})
	b.SetInvocation("run-2")
	b.Add(Entry{Unit: "a.service", Message: "second"})
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
	if len(entries) != 2 || entries[0].InvocationID != "run-1" || entries[1].InvocationID != "run-2" {
		t.Fatalf("stamped = %+v", entries)
	}
}
