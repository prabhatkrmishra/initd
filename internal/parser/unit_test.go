package parser

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestParseUnitService(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "foo.service", `
[Unit]
Description=Test service
After=network.target bar.service
Wants=baz.service
Requires=qux.service

[Service]
Type=simple
ExecStart=/bin/true
Restart=on-failure
RestartSec=2
WorkingDirectory=/srv/foo
Environment=FOO=bar
Environment=BAZ=qux

[Install]
WantedBy=multi-user.target
`)

	unit, err := ParseUnit(path)
	if err != nil {
		t.Fatalf("ParseUnit: %v", err)
	}

	if unit.Name != "foo.service" {
		t.Errorf("Name = %q, want foo.service", unit.Name)
	}
	if unit.Type != "service" {
		t.Errorf("Type = %q, want service", unit.Type)
	}
	if unit.Description != "Test service" {
		t.Errorf("Description = %q", unit.Description)
	}
	if len(unit.After) != 2 || unit.After[0] != "network.target" || unit.After[1] != "bar.service" {
		t.Errorf("After = %v", unit.After)
	}
	if len(unit.Wants) != 1 || unit.Wants[0] != "baz.service" {
		t.Errorf("Wants = %v", unit.Wants)
	}
	if len(unit.Requires) != 1 || unit.Requires[0] != "qux.service" {
		t.Errorf("Requires = %v", unit.Requires)
	}
	if unit.Service.Type != "simple" {
		t.Errorf("Service.Type = %q", unit.Service.Type)
	}
	if unit.Service.ExecStart != "/bin/true" {
		t.Errorf("ExecStart = %q", unit.Service.ExecStart)
	}
	if !unit.Service.ExecStartSet {
		t.Error("ExecStartSet = false, want true")
	}
	if unit.Service.Restart != "on-failure" {
		t.Errorf("Restart = %q", unit.Service.Restart)
	}
	if unit.Service.RestartSec != "2" {
		t.Errorf("RestartSec = %q", unit.Service.RestartSec)
	}
	if unit.Service.WorkingDirectory != "/srv/foo" {
		t.Errorf("WorkingDirectory = %q", unit.Service.WorkingDirectory)
	}
	if len(unit.Service.Environment) != 2 {
		t.Errorf("Environment = %v", unit.Service.Environment)
	}
	if len(unit.Install.WantedBy) != 1 || unit.Install.WantedBy[0] != "multi-user.target" {
		t.Errorf("WantedBy = %v", unit.Install.WantedBy)
	}
}

func TestParseUnitSocket(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "foo.socket", `
[Unit]
Description=Test socket

[Socket]
ListenStream=/run/foo.sock
SocketMode=0660

[Install]
WantedBy=sockets.target
`)

	unit, err := ParseUnit(path)
	if err != nil {
		t.Fatalf("ParseUnit: %v", err)
	}
	if unit.Type != "socket" {
		t.Errorf("Type = %q, want socket", unit.Type)
	}
	if len(unit.Socket.ListenStream) != 1 || unit.Socket.ListenStream[0] != "/run/foo.sock" {
		t.Errorf("ListenStream = %v", unit.Socket.ListenStream)
	}
	if unit.Socket.SocketMode != "0660" {
		t.Errorf("SocketMode = %q", unit.Socket.SocketMode)
	}
}

func TestParseUnitSocketMissingListen(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "bad.socket", `
[Unit]
Description=Bad socket

[Socket]
SocketMode=0660
`)
	if _, err := ParseUnit(path); err == nil {
		t.Error("expected error for socket unit missing ListenStream/ListenDatagram")
	}
}

func TestParseUnitUnknownType(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "foo.target", `
[Unit]
Description=A target
`)
	unit, err := ParseUnit(path)
	if err != nil {
		t.Fatalf("ParseUnit: %v", err)
	}
	if unit.Type != "unknown" {
		t.Errorf("Type = %q, want unknown", unit.Type)
	}
}

func TestParseUnitIgnoredKeys(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "foo.service", `
[Service]
Type=simple
ExecStart=/bin/true
MemoryMax=512M
PrivateTmp=yes
`)
	unit, err := ParseUnit(path)
	if err != nil {
		t.Fatalf("ParseUnit: %v", err)
	}
	if _, ok := unit.Ignored["MemoryMax"]; !ok {
		t.Error("MemoryMax should be recorded in Ignored")
	}
	if _, ok := unit.Ignored["PrivateTmp"]; !ok {
		t.Error("PrivateTmp should be recorded in Ignored")
	}
}

func TestParseUnitEmptyValueResetsList(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "foo.service", `
[Unit]
After=network.target
After=

[Service]
ExecStart=/bin/true
`)
	unit, err := ParseUnit(path)
	if err != nil {
		t.Fatalf("ParseUnit: %v", err)
	}
	if len(unit.After) != 0 {
		t.Errorf("After = %v, want empty after reset", unit.After)
	}
}

func TestParseUnitInvalidLine(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "foo.service", `
[Service]
ExecStart=/bin/true
this line has no equals
`)
	if _, err := ParseUnit(path); err == nil {
		t.Error("expected error for line without '='")
	}
}

func TestParseUnitWithDropins(t *testing.T) {
	dir := t.TempDir()
	searchDir := filepath.Join(dir, "system")
	writeFile(t, searchDir, "foo.service", `
[Unit]
Description=Base description
After=network.target

[Service]
Type=simple
ExecStart=/bin/true
Restart=no
`)
	// Drop-in override
	writeFile(t, filepath.Join(searchDir, "foo.service.d"), "override.conf", `
[Unit]
Description=Overridden description

[Service]
Restart=always
`)

	path := filepath.Join(searchDir, "foo.service")
	unit, err := ParseUnitWithDropins(path, []string{searchDir}, "")
	if err != nil {
		t.Fatalf("ParseUnitWithDropins: %v", err)
	}
	if unit.Description != "Overridden description" {
		t.Errorf("Description = %q, want overridden", unit.Description)
	}
	if unit.Service.Restart != "always" {
		t.Errorf("Restart = %q, want always", unit.Service.Restart)
	}
	// Non-overridden fields preserved
	if len(unit.After) != 1 || unit.After[0] != "network.target" {
		t.Errorf("After = %v, want preserved", unit.After)
	}
	if unit.Service.ExecStart != "/bin/true" {
		t.Errorf("ExecStart = %q, want preserved", unit.Service.ExecStart)
	}
}

func TestParseUnitWithDropinsAppendsLists(t *testing.T) {
	dir := t.TempDir()
	searchDir := filepath.Join(dir, "system")
	writeFile(t, searchDir, "foo.service", `
[Unit]
After=network.target

[Service]
ExecStart=/bin/true
`)
	writeFile(t, filepath.Join(searchDir, "foo.service.d"), "extra.conf", `
[Unit]
After=bar.service
`)
	path := filepath.Join(searchDir, "foo.service")
	unit, err := ParseUnitWithDropins(path, []string{searchDir}, "")
	if err != nil {
		t.Fatalf("ParseUnitWithDropins: %v", err)
	}
	if len(unit.After) != 2 {
		t.Errorf("After = %v, want appended list of 2", unit.After)
	}
}

func TestParseUnitWithDropinsEnabledRoot(t *testing.T) {
	dir := t.TempDir()
	searchDir := filepath.Join(dir, "system")
	enabledRoot := filepath.Join(dir, "enabled")
	writeFile(t, searchDir, "foo.service", `
[Service]
ExecStart=/bin/true
`)
	writeFile(t, filepath.Join(enabledRoot, "foo.service.d"), "override.conf", `
[Service]
Restart=on-failure
`)
	path := filepath.Join(searchDir, "foo.service")
	unit, err := ParseUnitWithDropins(path, []string{searchDir}, enabledRoot)
	if err != nil {
		t.Fatalf("ParseUnitWithDropins: %v", err)
	}
	if unit.Service.Restart != "on-failure" {
		t.Errorf("Restart = %q, want on-failure from enabled-root drop-in", unit.Service.Restart)
	}
}

func TestSplitList(t *testing.T) {
	got := splitList("  a   b\tc  ")
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("splitList = %v", got)
	}
	if got := splitList("   "); got != nil {
		t.Errorf("splitList(blank) = %v, want nil", got)
	}
}
