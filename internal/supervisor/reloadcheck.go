package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// unitFileID identifies a unit-related file by size + mtime. Cheap enough to
// recompute on every status call, precise enough to catch apt purge/install,
// edits and drop-in changes without hashing contents.
func unitFileID(path string) (string, bool) {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return "", false
	}
	return fmt.Sprintf("%d-%d", fi.Size(), fi.ModTime().UnixNano()), true
}

// currentUnitFilesFingerprint scans SearchPaths (+ EnabledRoot drop-ins) for
// *.service, *.socket and *.d/*.conf files. Keys are path-suffixed so a
// rename/replace is detected, values catch content changes.
func (m *Manager) currentUnitFilesFingerprint() map[string]string {
	fp := map[string]string{}
	dirs := append([]string{}, m.SearchPaths...)
	if m.EnabledRoot != "" {
		found := false
		for _, d := range dirs {
			if d == m.EnabledRoot {
				found = true
				break
			}
		}
		if !found {
			dirs = append(dirs, m.EnabledRoot)
		}
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			full := filepath.Join(dir, name)
			if e.IsDir() {
				if !strings.HasSuffix(name, ".d") {
					continue
				}
				subs, err := os.ReadDir(full)
				if err != nil {
					continue
				}
				for _, s := range subs {
					if s.IsDir() || !strings.HasSuffix(s.Name(), ".conf") {
						continue
					}
					sp := filepath.Join(full, s.Name())
					if id, ok := unitFileID(sp); ok {
						fp[sp] = id
					}
				}
				continue
			}
			if !strings.HasSuffix(name, ".service") && !strings.HasSuffix(name, ".socket") {
				continue
			}
			if id, ok := unitFileID(full); ok {
				fp[full] = id
			}
		}
	}
	return fp
}

// NeedDaemonReload reports whether unit files on disk differ from what the
// manager last loaded: added/removed units, edited files or changed
// drop-ins. Pure inequality check, safe to call on every status query.
func (m *Manager) NeedDaemonReload() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.currentUnitFilesFingerprint()
	if len(cur) != len(m.unitFilesFP) {
		return true
	}
	for k, v := range cur {
		if old, ok := m.unitFilesFP[k]; !ok || old != v {
			return true
		}
	}
	return false
}
