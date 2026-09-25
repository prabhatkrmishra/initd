package supervisor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"initd/internal/logging"
)

func TestOpenJournalWiresUnits(t *testing.T) {
	searchDir := t.TempDir()
	journalDir := t.TempDir()
	unitPath := filepath.Join(searchDir, "a.service")
	if err := os.WriteFile(unitPath, []byte("[Unit]\nDescription=A\n[Service]\nExecStart=/bin/true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewSystemManager()
	m.SearchPaths = []string{searchDir}
	m.EnabledRoot = t.TempDir()
	m.UserMode = true // skip tmpfiles; journal path comes from JournalDir
	m.JournalDir = journalDir
	m.journalBoot = "test-boot"
	m.journalHost = "testhost"
	if err := m.LoadUnits(); err != nil {
		t.Fatal(err)
	}
	if err := m.OpenJournal(); err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	defer m.CloseJournal()
	u, err := m.FindUnit("a.service")
	if err != nil {
		t.Fatal(err)
	}
	u.Log(logging.LevelInfo, "hello journal")
	m.CloseJournal()
	raw, err := os.ReadFile(filepath.Join(journalDir, "test-boot.jsonl"))
	if err != nil {
		t.Fatalf("journal file missing: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("journal file empty")
	}
}

func TestOpenJournalIsIdempotent(t *testing.T) {
	m := NewSystemManager()
	m.SearchPaths = []string{t.TempDir()}
	m.EnabledRoot = t.TempDir()
	m.UserMode = true
	m.JournalDir = t.TempDir()
	if err := m.OpenJournal(); err != nil {
		t.Fatal(err)
	}
	if err := m.OpenJournal(); err != nil {
		t.Fatalf("second OpenJournal: %v", err)
	}
	m.CloseJournal()
}

// journalManager opens a durable journal over an empty dir for vacuum tests.
func journalManager(t *testing.T, dir string) *Manager {
	t.Helper()
	m := NewSystemManager()
	m.SearchPaths = []string{t.TempDir()}
	m.EnabledRoot = t.TempDir()
	m.UserMode = true
	m.JournalDir = dir
	m.journalBoot = "vacuum-boot"
	m.journalHost = "testhost"
	if err := m.OpenJournal(); err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	t.Cleanup(m.CloseJournal)
	return m
}

func writeJournal(t *testing.T, m *Manager, msg string) {
	t.Helper()
	if err := m.journal.Append(logging.StoredEntry{Unit: "a.service", Message: msg}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := m.journal.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}

// A unit that logged once and went quiet pins its file with the freshest mtime,
// so age expiry must close it before it can expire the history inside.
func TestVacuumExpiresHistoryInTheOpenFile(t *testing.T) {
	dir := t.TempDir()
	m := journalManager(t, dir)
	writeJournal(t, m, "thirty days old")
	stale := time.Now().AddDate(0, 0, -30)
	if err := os.Chtimes(m.journal.ActivePath(), stale, stale); err != nil {
		t.Fatal(err)
	}

	if err := m.VacuumJournal(20<<20, 10, 7); err != nil {
		t.Fatalf("VacuumJournal: %v", err)
	}
	files := m.JournalFiles()
	if len(files) == 0 {
		t.Fatal("vacuum deleted every journal file")
	}
	if _, err := os.Stat(m.journal.ActivePath()); err != nil {
		t.Fatalf("the writer's own file disappeared: %v", err)
	}
	entries, err := logging.ReadAll(files)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Message == "thirty days old" {
			t.Fatal("history older than the age bound survived the vacuum")
		}
	}
}

// The rotate is a means, not a side effect: a current journal must come back
// with the same single file it went in with.
func TestVacuumLeavesAFreshJournalAlone(t *testing.T) {
	dir := t.TempDir()
	m := journalManager(t, dir)
	writeJournal(t, m, "today")

	if err := m.VacuumJournal(20<<20, 10, 14); err != nil {
		t.Fatalf("VacuumJournal: %v", err)
	}
	files := m.JournalFiles()
	if len(files) != 1 {
		t.Fatalf("files = %d, want the one fresh file with no empty generation added", len(files))
	}
	if filepath.Base(files[0]) != "vacuum-boot.jsonl" {
		t.Fatalf("files = %v, want the active file", files)
	}
	entries, _ := logging.ReadAll(files)
	if len(entries) != 1 || entries[0].Message != "today" {
		t.Fatalf("entries = %+v, want the fresh line kept", entries)
	}
}

// Tight size and file bounds must still leave readable history: the only file
// on disk is the one the writer holds.
func TestVacuumKeepsTheLastFile(t *testing.T) {
	dir := t.TempDir()
	m := journalManager(t, dir)
	for i := 0; i < 200; i++ {
		if err := m.journal.Append(logging.StoredEntry{Unit: "a.service", Message: "line to fill the journal file so a vacuum has work to do"}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := m.journal.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if err := m.VacuumJournal(1, 1, 1); err != nil {
		t.Fatalf("VacuumJournal: %v", err)
	}
	files := m.JournalFiles()
	if len(files) == 0 {
		t.Fatal("vacuum left no journal file at all")
	}
	entries, _ := logging.ReadAll(files)
	if len(entries) == 0 {
		t.Fatal("vacuum wiped history instead of keeping the newest generation")
	}
}
