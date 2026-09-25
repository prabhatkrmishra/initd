package supervisor

import (
	"os"
	"time"

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
	// Retention caps so a chatty unit cannot grow the journal without bound
	// between explicit vacuums. Mirrors VacuumJournal's defaults: user scope
	// keeps ~20MB, system scope ~100MB, both at most 10 files. Age expiry
	// stays manual (`--vacuum-time`); rotation enforces size/count and the
	// startup guard below trims any pre-existing backlog, never the active
	// file in either case.
	if m.UserMode {
		w.SetRetention(10, 20<<20)
	} else {
		w.SetRetention(10, 100<<20)
	}
	// The rotation hook only trims on future rotates, so a daemon
	// restarting into a 1GB backlog would serve it whole once first.
	// Enforce the caps now, best-effort, before wiring unit rings.
	w.EnforceRetention()
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

// JournalUsage totals durable files: bytes and file count, like
// journalctl --disk-usage in terse form.
//
// The dir is included because the client's environment (HOME, XDG_STATE_HOME)
// can resolve to a different path than the daemon's — the daemon's answer
// is the one that matters.
func (m *Manager) JournalUsage() map[string]int64 {
	var bytes int64
	var files int64
	for _, path := range m.JournalFiles() {
		if st, err := os.Stat(path); err == nil {
			bytes += st.Size()
			files++
		}
	}
	return map[string]int64{"bytes": bytes, "files": files}
}

// ActiveJournalDir reports the durable dir this manager actually writes to.
// The client prefers it over recomputing from its own environment.
func (m *Manager) ActiveJournalDir() string {
	return m.defaultJournalDir()
}

// VacuumJournal drops old generations until bytes/files/age fit. Zero means
// no bound on that axis. Defaults mirror the plan: 100M system, 20M user,
// 10 files, 14 days — enough to matter on small disks, quiet otherwise.
func (m *Manager) VacuumJournal(maxBytes int64, maxFiles int, maxAgeDays int) error {
	if maxBytes <= 0 {
		if m.UserMode {
			maxBytes = 20 << 20
		} else {
			maxBytes = 100 << 20
		}
	}
	if maxFiles <= 0 {
		maxFiles = 10
	}
	if maxAgeDays <= 0 {
		maxAgeDays = 14
	}
	cutoff := time.Now().AddDate(0, 0, -maxAgeDays)
	m.mu.Lock()
	w := m.journal
	m.mu.Unlock()
	// The active file always carries the freshest mtime, so age expiry could
	// never reach it. Close it into a dated generation the pass can see.
	if w != nil {
		if _, err := w.RotateIfStale(cutoff); err != nil {
			return err
		}
	}
	files := m.JournalFiles()
	if len(files) == 0 {
		return nil
	}
	// Age pass. The writer's file and the newest one on disk are spared, so a
	// vacuum never leaves an empty journal.
	aged := map[string]bool{files[len(files)-1]: true}
	if w != nil {
		aged[w.ActivePath()] = true
	}
	remove := func(path string) {
		_ = os.Remove(path)
	}
	for _, path := range files {
		if aged[path] {
			continue
		}
		if st, err := os.Stat(path); err == nil && st.ModTime().Before(cutoff) {
			remove(path)
		}
	}
	files = m.JournalFiles()
	if len(files) == 0 {
		return nil
	}
	// Never delete the newest file: the writer may hold it open.
	keepNewest := files[len(files)-1]
	// After a rotate the newest file is empty while the previous one
	// holds all history. Deleting that previous file to satisfy a small
	// --vacuum-size would wipe everything and leave 0B. Keep the newest
	// non-empty file as well so size/file-count vacuums leave data
	// behind instead of an empty active file.
	kept := map[string]bool{keepNewest: true}
	if st, err := os.Stat(keepNewest); err == nil && st.Size() < 4096 && len(files) >= 2 {
		for i := len(files) - 2; i >= 0; i-- {
			if st, err := os.Stat(files[i]); err == nil && st.Size() >= 4096 {
				kept[files[i]] = true
				break
			}
		}
		// No non-empty predecessor (all tiny): keep the immediate
		// predecessor so a fresh rotate never collapses to one empty file.
		if len(kept) == 1 {
			kept[files[len(files)-2]] = true
		}
	}
	for len(files) > maxFiles {
		oldest := ""
		for _, path := range files {
			if kept[path] {
				continue
			}
			oldest = path
			break
		}
		if oldest == "" {
			break
		}
		remove(oldest)
		files = m.JournalFiles()
	}
	var total int64
	for _, path := range m.JournalFiles() {
		if st, err := os.Stat(path); err == nil {
			total += st.Size()
		}
	}
	for total > maxBytes {
		oldest := ""
		for _, path := range m.JournalFiles() {
			if kept[path] {
				continue
			}
			oldest = path
			break
		}
		if oldest == "" {
			break
		}
		remove(oldest)
		total = 0
		for _, path := range m.JournalFiles() {
			if st, err := os.Stat(path); err == nil {
				total += st.Size()
			}
		}
	}
	return nil
}
