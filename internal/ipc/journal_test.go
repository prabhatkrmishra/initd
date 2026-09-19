package ipc

import (
	"testing"

	"initd/internal/logging"
	"initd/internal/supervisor"
)

func journalTestManager(t *testing.T) *supervisor.Manager {
	t.Helper()
	m := supervisor.NewSystemManager()
	m.SearchPaths = []string{t.TempDir()}
	m.EnabledRoot = t.TempDir()
	m.UserMode = true
	m.JournalDir = t.TempDir()
	if err := m.LoadUnits(); err != nil {
		t.Fatal(err)
	}
	if err := m.OpenJournal(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.CloseJournal)
	return m
}

func TestDispatchJournalFilters(t *testing.T) {
	m := journalTestManager(t)
	// Seed through the file writer directly: same bytes the daemon writes.
	dir := m.JournalFiles()
	_ = dir
	if err := m.SyncJournal(); err != nil {
		t.Fatal(err)
	}
	resp := dispatch(Request{Action: "journal", Units: []string{"missing.service"}}, m)
	if !resp.Success {
		t.Fatalf("journal: %v", resp.Message)
	}
	entries, ok := resp.Data.([]logging.StoredEntry)
	if !ok {
		t.Fatalf("journal Data = %T", resp.Data)
	}
	if len(entries) != 0 {
		t.Fatalf("journal for missing unit = %d, want 0", len(entries))
	}
	resp = dispatch(Request{Action: "journal-boots"}, m)
	if !resp.Success {
		t.Fatalf("journal-boots: %v", resp.Message)
	}
	resp = dispatch(Request{Action: "journal-usage"}, m)
	if !resp.Success {
		t.Fatalf("journal-usage: %v", resp.Message)
	}
	usage, ok := resp.Data.(map[string]int64)
	if !ok {
		t.Fatalf("usage Data = %T", resp.Data)
	}
	if _, ok := usage["bytes"]; !ok {
		t.Fatalf("usage missing bytes: %+v", usage)
	}
	for _, act := range []string{"journal-sync", "journal-rotate", "journal-vacuum"} {
		if resp := dispatch(Request{Action: act}, m); !resp.Success {
			t.Fatalf("%s: %v", act, resp.Message)
		}
	}
}
