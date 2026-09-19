package logging

import "testing"

func seedEntries() []StoredEntry {
	return []StoredEntry{
		{Seq: 1, Cursor: "c1", RealtimeUsec: 1000, BootID: "b1", Unit: "a.service", Priority: 6, Identifier: "a", Message: "hello world"},
		{Seq: 2, Cursor: "c2", RealtimeUsec: 2000, BootID: "b1", Unit: "a.service", Priority: 3, Identifier: "a", Message: "boom failure"},
		{Seq: 3, Cursor: "c3", RealtimeUsec: 3000, BootID: "b2", Unit: "b.service", Priority: 6, Identifier: "b", Message: "other hello"},
		{Seq: 4, Cursor: "c4", RealtimeUsec: 4000, BootID: "b2", Unit: "a.service", Priority: 4, Identifier: "a", Message: "HELLO again"},
	}
}

func TestQueryUnits(t *testing.T) {
	out := QueryJournal(seedEntries(), JournalFilter{Units: []string{"a.service"}})
	if len(out) != 3 {
		t.Fatalf("units = %d, want 3", len(out))
	}
	out = QueryJournal(seedEntries(), JournalFilter{Units: []string{"a"}})
	if len(out) != 3 {
		t.Fatalf("short name should expand, got %d", len(out))
	}
}

func TestQueryBootTimePriority(t *testing.T) {
	out := QueryJournal(seedEntries(), JournalFilter{BootID: "b1"})
	if len(out) != 2 {
		t.Fatalf("boot = %d, want 2", len(out))
	}
	out = QueryJournal(seedEntries(), JournalFilter{SinceUsec: 2000, UntilUsec: 3000})
	if len(out) != 2 {
		t.Fatalf("time window = %d, want 2", len(out))
	}
	out = QueryJournal(seedEntries(), JournalFilter{PriorityMax: 3, PrioritySet: true})
	if len(out) != 1 || out[0].Message != "boom failure" {
		t.Fatalf("priority = %+v", out)
	}
}

func TestQueryGrepIdentifier(t *testing.T) {
	out := QueryJournal(seedEntries(), JournalFilter{Grep: "hello"})
	if len(out) != 3 {
		t.Fatalf("case-insensitive grep = %d, want 3", len(out))
	}
	out = QueryJournal(seedEntries(), JournalFilter{Grep: "HELLO", CaseSensitive: true})
	if len(out) != 1 {
		t.Fatalf("case-sensitive grep = %d, want 1", len(out))
	}
	out = QueryJournal(seedEntries(), JournalFilter{Identifier: "B"})
	if len(out) != 1 {
		t.Fatalf("identifier = %d, want 1", len(out))
	}
}

func TestQueryCursorLinesReverse(t *testing.T) {
	out := QueryJournal(seedEntries(), JournalFilter{Cursor: "c2"})
	if len(out) != 3 || out[0].Cursor != "c2" {
		t.Fatalf("cursor = %+v", out)
	}
	out = QueryJournal(seedEntries(), JournalFilter{Cursor: "c2", CursorAfter: true})
	if len(out) != 2 || out[0].Cursor != "c3" {
		t.Fatalf("after-cursor = %+v", out)
	}
	if out := QueryJournal(seedEntries(), JournalFilter{Cursor: "missing"}); out != nil {
		t.Fatalf("unknown cursor should be empty, got %+v", out)
	}
	out = QueryJournal(seedEntries(), JournalFilter{Lines: 2})
	if len(out) != 2 || out[0].Cursor != "c3" {
		t.Fatalf("tail = %+v", out)
	}
	out = QueryJournal(seedEntries(), JournalFilter{Reverse: true})
	if len(out) != 4 || out[0].Cursor != "c4" {
		t.Fatalf("reverse = %+v", out)
	}
}

func TestBootList(t *testing.T) {
	boots := BootList(seedEntries())
	if len(boots) != 2 || boots[0][0] != "b1" || boots[0][1] != "2" || boots[1][1] != "2" {
		t.Fatalf("boots = %+v", boots)
	}
}
