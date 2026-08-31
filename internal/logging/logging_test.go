package logging

import (
	"strings"
	"testing"
	"time"
)

func TestBufferAddAndEntries(t *testing.T) {
	b := NewBuffer(3)
	b.Add(Entry{Timestamp: 1, Unit: "a", PID: 1, Level: LevelInfo, Message: "one"})
	b.Add(Entry{Timestamp: 2, Unit: "a", PID: 1, Level: LevelInfo, Message: "two"})
	b.Add(Entry{Timestamp: 3, Unit: "a", PID: 1, Level: LevelInfo, Message: "three"})
	// Fourth entry evicts the oldest.
	b.Add(Entry{Timestamp: 4, Unit: "a", PID: 1, Level: LevelInfo, Message: "four"})

	entries := b.Entries()
	if len(entries) != 3 {
		t.Fatalf("Entries len = %d, want 3", len(entries))
	}
	if entries[0].Message != "two" {
		t.Errorf("entries[0].Message = %q, want two (oldest evicted)", entries[0].Message)
	}
	if entries[2].Message != "four" {
		t.Errorf("entries[2].Message = %q, want four", entries[2].Message)
	}
}

func TestBufferMaxZero(t *testing.T) {
	b := NewBuffer(0)
	b.Add(Entry{Message: "x"})
	if len(b.Entries()) != 0 {
		t.Error("buffer with max 0 should not store entries")
	}
}

func TestFormatEntry(t *testing.T) {
	entry := Entry{Timestamp: 123 * time.Millisecond, Unit: "foo.service", PID: 42, Level: LevelInfo, Message: "hello"}
	got := FormatEntry(entry)
	if !strings.Contains(got, "foo.service") {
		t.Errorf("FormatEntry missing unit: %q", got)
	}
	if !strings.Contains(got, "[42]") {
		t.Errorf("FormatEntry missing pid: %q", got)
	}
	if !strings.Contains(got, "hello") {
		t.Errorf("FormatEntry missing message: %q", got)
	}
}

func TestLineLoggerWrite(t *testing.T) {
	b := NewBuffer(10)
	l := &LineLogger{Unit: "foo.service", PID: 7, Level: LevelInfo, Buffer: b}
	n, err := l.Write([]byte("line one\nline two\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len("line one\nline two\n") {
		t.Errorf("Write n = %d", n)
	}
	entries := b.Entries()
	if len(entries) != 2 {
		t.Fatalf("Entries len = %d, want 2", len(entries))
	}
	if entries[0].Message != "line one" {
		t.Errorf("entries[0].Message = %q", entries[0].Message)
	}
	if entries[1].Message != "line two" {
		t.Errorf("entries[1].Message = %q", entries[1].Message)
	}
	if entries[0].Unit != "foo.service" || entries[0].PID != 7 {
		t.Errorf("entry metadata = %+v", entries[0])
	}
}
