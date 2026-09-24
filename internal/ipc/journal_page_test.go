package ipc

import (
	"testing"

	"initd/internal/logging"
)

// A head-after-cursor journal request must take the streaming path and page
// identically to the materializing query over the same files.
func TestDispatchJournalPaged(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "app.service", "[Service]\nExecStart=/bin/sleep 30\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatal(err)
	}
	m.JournalDir = t.TempDir()
	if err := m.OpenJournal(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.CloseJournal)
	u, err := m.FindUnit("app.service")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		u.Log(logging.LevelInfo, "paged-line")
	}
	if err := m.SyncJournal(); err != nil {
		t.Fatal(err)
	}
	asEntries := func(resp Response) []logging.StoredEntry {
		t.Helper()
		if !resp.Success {
			t.Fatalf("journal: %v", resp.Message)
		}
		// dispatch returns []logging.StoredEntry for journal.
		entries, ok := resp.Data.([]logging.StoredEntry)
		if !ok {
			t.Fatalf("Data = %T", resp.Data)
		}
		return entries
	}
	full := asEntries(dispatch(Request{Action: "journal", Units: []string{"app.service"}}, m))
	if len(full) != 30 {
		t.Fatalf("full = %d, want 30", len(full))
	}
	var got []logging.StoredEntry
	cursor, after := "", false
	for {
		page := asEntries(dispatch(Request{
			Action: "journal", Units: []string{"app.service"},
			Lines: 7, LinesPlus: true, Cursor: cursor, CursorAfter: after,
		}, m))
		if len(page) == 0 {
			break
		}
		got = append(got, page...)
		cursor, after = page[len(page)-1].Cursor, true
		if len(page) < 7 {
			break
		}
	}
	if len(got) != len(full) {
		t.Fatalf("paged %d != full %d", len(got), len(full))
	}
	for i := range got {
		if got[i].Cursor != full[i].Cursor {
			t.Fatalf("entry %d cursor differs", i)
		}
	}
}
