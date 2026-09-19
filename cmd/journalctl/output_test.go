package main

import (
	"strings"
	"testing"

	"initd/internal/logging"
)

func sampleEntry() logging.StoredEntry {
	return logging.StoredEntry{
		Seq: 1, Cursor: "c1", RealtimeUsec: 1726740000000000,
		BootID: "b1", Unit: "foo.service", PID: 42, Priority: 3,
		Identifier: "foo", Hostname: "h", Message: "boom",
	}
}

func TestOutputModes(t *testing.T) {
	e := sampleEntry()
	if got := formatEntries([]logging.StoredEntry{e}, "cat", false, false, ""); len(got) != 1 || got[0] != "boom" {
		t.Fatalf("cat = %v", got)
	}
	if got := formatEntries([]logging.StoredEntry{e}, "bogus-mode", false, false, ""); len(got) != 1 || !strings.Contains(got[0], "boom") {
		t.Fatalf("unknown mode should fall back to short: %v", got)
	}
	if got := formatEntries([]logging.StoredEntry{e}, "json", false, false, ""); !strings.Contains(got[0], `"MESSAGE":"boom"`) {
		t.Fatalf("json = %v", got)
	}
	if got := formatEntries([]logging.StoredEntry{e}, "json", false, false, "MESSAGE"); strings.Contains(got[0], "PRIORITY") || !strings.Contains(got[0], "MESSAGE") {
		t.Fatalf("field filter = %v", got)
	}
	if got := formatEntries([]logging.StoredEntry{e}, "export", false, false, ""); !strings.Contains(got[0], "MESSAGE=boom") {
		t.Fatalf("export = %v", got)
	}
	if got := formatEntries([]logging.StoredEntry{e}, "short-unix", false, false, ""); !strings.HasPrefix(got[0], "1726740000 ") {
		t.Fatalf("short-unix = %v", got)
	}
}
