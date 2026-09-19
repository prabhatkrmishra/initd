package supervisor

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeReloadUnit(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNeedDaemonReload(t *testing.T) {
	dir := t.TempDir()
	writeReloadUnit(t, dir, "a.service", "[Unit]\nDescription=A\n[Service]\nExecStart=/bin/true\n")

	m := NewSystemManager()
	m.SearchPaths = []string{dir}
	m.EnabledRoot = t.TempDir()
	if err := m.LoadUnits(); err != nil {
		t.Fatal(err)
	}
	if m.NeedDaemonReload() {
		t.Fatal("fresh load should not need reload")
	}

	// Delete the unit file: stale listing until reload.
	if err := os.Remove(filepath.Join(dir, "a.service")); err != nil {
		t.Fatal(err)
	}
	if !m.NeedDaemonReload() {
		t.Fatal("deleted unit file should need reload")
	}
	if err := m.LoadUnits(); err != nil {
		t.Fatal(err)
	}
	if m.NeedDaemonReload() {
		t.Fatal("reload after delete should clear flag")
	}

	// Re-add then edit: mtime/size change must trip the flag.
	writeReloadUnit(t, dir, "a.service", "[Unit]\nDescription=A\n[Service]\nExecStart=/bin/true\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	writeReloadUnit(t, dir, "a.service", "[Unit]\nDescription=A changed\n[Service]\nExecStart=/bin/true --extra\n")
	if !m.NeedDaemonReload() {
		t.Fatal("edited unit file should need reload")
	}

	// Drop-in change must trip the flag too.
	if err := m.LoadUnits(); err != nil {
		t.Fatal(err)
	}
	if m.NeedDaemonReload() {
		t.Fatal("reload after edit should clear flag")
	}
	if err := os.MkdirAll(filepath.Join(dir, "a.service.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeReloadUnit(t, filepath.Join(dir, "a.service.d"), "override.conf", "[Service]\nRestart=always\n")
	if !m.NeedDaemonReload() {
		t.Fatal("new drop-in should need reload")
	}
}
