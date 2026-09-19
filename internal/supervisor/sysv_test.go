package supervisor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"initd/internal/service"
)

func writeSysVScript(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

const sysvLSBScript = `#!/bin/sh
### BEGIN INIT INFO
# Provides:          mydaemon
# Short-Description: My test daemon
# Description:       Longer description here
### END INIT INFO
case "$1" in
  start) echo starting ;;
  stop) echo stopping ;;
  restart) echo restarting ;;
esac
`

func TestSysVGeneratedUnit(t *testing.T) {
	sysvDir := t.TempDir()
	writeSysVScript(t, sysvDir, "mydaemon", sysvLSBScript)

	m := NewSystemManager()
	m.SearchPaths = []string{t.TempDir()}
	m.EnabledRoot = t.TempDir()
	m.UserMode = false
	m.SysVInitDir = sysvDir
	if err := m.LoadUnits(); err != nil {
		t.Fatal(err)
	}
	u, err := m.FindUnit("mydaemon.service")
	if err != nil {
		t.Fatalf("generated unit not found: %v", err)
	}
	if u.Description() != "My test daemon" {
		t.Fatalf("description = %q, want LSB short description", u.Description())
	}
	if u.Config.GeneratedFrom == "" {
		t.Fatal("GeneratedFrom should point at the init script")
	}
	if u.Config.Service.Type != "oneshot" || u.Config.Service.RemainAfterExit != "yes" {
		t.Fatalf("wrap = %q/%q, want oneshot/yes", u.Config.Service.Type, u.Config.Service.RemainAfterExit)
	}
}

func TestSysVNativeWins(t *testing.T) {
	searchDir := t.TempDir()
	sysvDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(searchDir, "mydaemon.service"), []byte("[Unit]\nDescription=Native\n[Service]\nExecStart=/bin/true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSysVScript(t, sysvDir, "mydaemon", sysvLSBScript)

	m := NewSystemManager()
	m.SearchPaths = []string{searchDir}
	m.EnabledRoot = t.TempDir()
	m.UserMode = false
	m.SysVInitDir = sysvDir
	if err := m.LoadUnits(); err != nil {
		t.Fatal(err)
	}
	u, err := m.FindUnit("mydaemon.service")
	if err != nil {
		t.Fatal(err)
	}
	if u.Description() != "Native" {
		t.Fatalf("native unit should win, got %q", u.Description())
	}
	if u.Config.GeneratedFrom != "" {
		t.Fatal("native unit must not carry GeneratedFrom")
	}
}

func TestSysVStartStop(t *testing.T) {
	sysvDir := t.TempDir()
	state := filepath.Join(sysvDir, "state")
	writeSysVScript(t, sysvDir, "mydaemon", "#!/bin/sh\ncase \"$1\" in\n start) echo on > "+state+" ;;\n stop) rm -f "+state+" ;;\n restart) echo on > "+state+" ;;\nesac\n")

	m := NewSystemManager()
	m.SearchPaths = []string{t.TempDir()}
	m.EnabledRoot = t.TempDir()
	m.UserMode = false
	m.SysVInitDir = sysvDir
	if err := m.LoadUnits(); err != nil {
		t.Fatal(err)
	}
	u, err := m.FindUnit("mydaemon.service")
	if err != nil {
		t.Fatalf("FindUnit: %v", err)
	}
	if _, err := u.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for u.Snapshot().State == service.StateActivating && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("script start was not executed: %v", err)
	}
	if got := u.Snapshot().State; got != "active" {
		t.Fatalf("state after start = %s, want active (exited)", got)
	}
	if !u.RemainActive() {
		t.Fatal("oneshot RemainAfterExit should report RemainActive")
	}
	if got := u.SubState(); string(got) != "exited" {
		t.Fatalf("substate = %q, want exited", got)
	}
	if err := m.StopUnit("mydaemon.service"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("script stop was not executed")
	}
}

func TestSysVSkipsNonScripts(t *testing.T) {
	sysvDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sysvDir, "README"), []byte("docs"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysvDir, "notexec"), []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewSystemManager()
	m.UserMode = false
	m.SearchPaths = []string{t.TempDir()}
	m.EnabledRoot = t.TempDir()
	m.SysVInitDir = sysvDir
	if err := m.LoadUnits(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.FindUnit("README.service"); err == nil {
		t.Fatal("README must not become a unit")
	}
	if _, err := m.FindUnit("notexec.service"); err == nil {
		t.Fatal("non-executable must not become a unit")
	}
}
