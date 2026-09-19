package main

import (
	"testing"

	"initd/internal/logging"
)

func TestApplyDisplayFiltersQuiet(t *testing.T) {
	entries := []logging.StoredEntry{
		{Message: "info line", Priority: 6},
		{Message: "warning line", Priority: 4},
		{Message: "error line", Priority: 3},
	}
	out := applyDisplayFilters(entries, journalOpts{quiet: true})
	if len(out) != 2 {
		t.Fatalf("quiet kept %d, want 2 (warning and below)", len(out))
	}
	for _, e := range out {
		if e.Priority > 4 {
			t.Errorf("quiet leaked priority %d", e.Priority)
		}
	}
}

func TestApplyDisplayFiltersTruncate(t *testing.T) {
	entries := []logging.StoredEntry{{Message: "first\nsecond", Priority: 3}}
	out := applyDisplayFilters(entries, journalOpts{truncateNewline: true})
	if len(out) != 1 || out[0].Message != "first" {
		t.Fatalf("truncate = %+v", out)
	}
	// No flags: passthrough, original slice untouched.
	orig := []logging.StoredEntry{{Message: "a\nb", Priority: 6}}
	if out := applyDisplayFilters(orig, journalOpts{}); len(out) != 1 || out[0].Message != "a\nb" {
		t.Fatalf("passthrough = %+v", out)
	}
}
