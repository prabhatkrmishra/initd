package supervisor

import (
	"os"
	"path/filepath"
	"testing"

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
