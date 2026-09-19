package main

import (
	"testing"
	"time"
)

func TestParseSinceUntil(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	got, err := parseSinceUntil("--since", "2026-09-18 10:00", now)
	if err != nil {
		t.Fatalf("datetime: %v", err)
	}
	want := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC).UnixMicro()
	if got != want {
		t.Fatalf("datetime = %d, want %d", got, want)
	}
	got, err = parseSinceUntil("--since", "yesterday", now)
	if err != nil {
		t.Fatalf("yesterday: %v", err)
	}
	if want := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC).UnixMicro(); got != want {
		t.Fatalf("yesterday = %d, want %d", got, want)
	}
	got, err = parseSinceUntil("--since", "-2h", now)
	if err != nil {
		t.Fatalf("relative: %v", err)
	}
	if want := now.Add(-2 * time.Hour).UnixMicro(); got != want {
		t.Fatalf("relative = %d, want %d", got, want)
	}
	if _, err := parseSinceUntil("--since", "not a date", now); err == nil {
		t.Fatal("garbage should fail")
	}
}

func TestParsePriority(t *testing.T) {
	p, set, err := parsePriority("err")
	if err != nil || !set || p != 3 {
		t.Fatalf("err = %d,%v,%v", p, set, err)
	}
	p, set, err = parsePriority("3..5")
	if err != nil || !set || p != 5 {
		t.Fatalf("range = %d,%v,%v", p, set, err)
	}
	if _, _, err := parsePriority("bogus"); err == nil {
		t.Fatal("bogus priority should fail")
	}
	if _, _, err := parsePriority("9"); err == nil {
		t.Fatal("priority 9 should fail")
	}
}
