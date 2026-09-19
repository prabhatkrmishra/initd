package supervisor

import (
	"os"

	"initd/internal/logging"
	"initd/internal/service"
	"initd/internal/userpaths"
)

// defaultJournalDir resolves the durable log dir for this manager scope.
func (m *Manager) defaultJournalDir() string {
	if m.JournalDir != "" {
		return m.JournalDir
	}
	if m.UserMode {
		return userpaths.UserJournalDir()
	}
	return userpaths.SystemJournalDir()
}

func (m *Manager) journalBootID() string {
	if m.journalBoot != "" {
		return m.journalBoot
	}
	return logging.BootID()
}

func (m *Manager) journalHostname() string {
	if m.journalHost != "" {
		return m.journalHost
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "localhost"
}

// OpenJournal opens the durable per-boot store and wires every loaded unit's
// ring to it. Idempotent: a second call is a no-op. A failure to open (e.g.
// read-only /var/log) leaves RAM-only rings and reports the error so the
// daemon keeps supervising instead of exiting.
func (m *Manager) OpenJournal() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.journal != nil {
		return nil
	}
	w, err := logging.NewFileWriter(m.defaultJournalDir(), m.journalBootID(), m.journalHostname(), logging.DefaultMaxFileBytes)
	if err != nil {
		return err
	}
	m.journal = w
	for _, u := range m.Units {
		u.Logs.AttachFile(w)
	}
	return nil
}

// CloseJournal flushes and detaches the durable store without touching unit
// supervision. Used on shutdown and in tests.
func (m *Manager) CloseJournal() {
	m.mu.Lock()
	w := m.journal
	m.journal = nil
	for _, u := range m.Units {
		u.Logs.AttachFile(nil)
	}
	m.mu.Unlock()
	if w != nil {
		_ = w.Sync()
		_ = w.Close()
	}
}

// attachJournalLocked wires a freshly created unit to the open store. The
// caller holds m.mu; attach itself never blocks on disk.
func (m *Manager) attachJournalLocked(u *service.Unit) {
	if m.journal != nil && u != nil {
		u.Logs.AttachFile(m.journal)
	}
}

// JournalFiles lists durable files for reads/vacuum, oldest first.
func (m *Manager) JournalFiles() []string {
	return logging.ListFiles(m.defaultJournalDir())
}

// SyncJournal flushes buffered lines. journalctl --sync maps here.
func (m *Manager) SyncJournal() error {
	m.mu.Lock()
	w := m.journal
	m.mu.Unlock()
	if w == nil {
		return nil
	}
	return w.Sync()
}

// RotateJournal forces a new generation. journalctl --rotate maps here.
func (m *Manager) RotateJournal() error {
	m.mu.Lock()
	w := m.journal
	m.mu.Unlock()
	if w == nil {
		return nil
	}
	return w.Rotate()
}
