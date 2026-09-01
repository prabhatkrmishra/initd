package dbus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"initd/internal/supervisor"
)

func newTestManager(t *testing.T) *supervisor.Manager {
	t.Helper()
	dir := t.TempDir()
	searchDir := filepath.Join(dir, "system")
	enabledRoot := filepath.Join(dir, "enabled")
	if err := os.MkdirAll(searchDir, 0o755); err != nil {
		t.Fatalf("mkdir search: %v", err)
	}
	if err := os.MkdirAll(enabledRoot, 0o755); err != nil {
		t.Fatalf("mkdir enabled: %v", err)
	}
	m := supervisor.NewSystemManager()
	m.SearchPaths = []string{searchDir}
	m.EnabledRoot = enabledRoot
	m.UserMode = true
	return m
}

func writeUnitForDBus(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestUnitObjectPathEscaping(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"foo.service", "/org/freedesktop/systemd1/unit/foo_2eservice"},
		{"openclaw-gateway.service", "/org/freedesktop/systemd1/unit/openclaw_2dgateway_2eservice"},
		{"bar@baz.service", "/org/freedesktop/systemd1/unit/bar_40baz_2eservice"},
	}
	for _, tc := range cases {
		got := string(unitObjectPath(tc.name))
		if got != tc.want {
			t.Fatalf("unitObjectPath(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestBuildUnitPropsNotFound(t *testing.T) {
	mgr := newTestManager(t)
	props := buildUnitProps(mgr, "nonexistent.service")
	if props != nil {
		t.Fatalf("expected nil for nonexistent unit")
	}
}

func TestBuildUnitPropsKnownUnit(t *testing.T) {
	mgr := newTestManager(t)
	searchDir := mgr.SearchPaths[0]
	writeUnitForDBus(t, searchDir, "hello.service", "[Unit]\nDescription=Hello\n[Service]\nExecStart=/bin/true\n")
	if err := mgr.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	props := buildUnitProps(mgr, "hello.service")
	if props == nil {
		t.Fatalf("expected non-nil props for known unit")
	}
	if v, ok := props["Id"]; !ok {
		t.Fatalf("missing Id prop")
	} else if v.Value != "hello.service" {
		t.Fatalf("Id=%v, want hello.service", v.Value)
	}
	if v, ok := props["ActiveState"]; !ok {
		t.Fatalf("missing ActiveState prop")
	} else if v.Value == nil {
		t.Fatalf("ActiveState value nil")
	}
}

func TestManagerGetUnitErrors(t *testing.T) {
	mgr := newTestManager(t)
	searchDir := mgr.SearchPaths[0]
	writeUnitForDBus(t, searchDir, "hello.service", "[Unit]\nDescription=Hello\n[Service]\nExecStart=/bin/true\n")
	if err := mgr.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	m := newManager(mgr)

	_, derr := m.GetUnit("")
	if derr == nil {
		t.Fatalf("expected error for empty name")
	}

	_, derr = m.GetUnit("nonexistent.service")
	if derr == nil {
		t.Fatalf("expected error for not-found unit")
	}
	if !strings.Contains(derr.Name, "NoSuchUnit") {
		t.Fatalf("expected NoSuchUnit error, got %v", derr)
	}

	path, derr := m.GetUnit("hello.service")
	if derr != nil {
		t.Fatalf("GetUnit(hello) error: %v", derr)
	}
	if !strings.HasPrefix(string(path), "/org/freedesktop/systemd1/unit/") {
		t.Fatalf("bad path %q", path)
	}
}

func TestManagerGetUnitFileState(t *testing.T) {
	mgr := newTestManager(t)
	searchDir := mgr.SearchPaths[0]
	writeUnitForDBus(t, searchDir, "hello.service", "[Unit]\nDescription=Hello\n[Service]\nExecStart=/bin/true\n[Install]\nWantedBy=multi-user.target\n")
	if err := mgr.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	m := newManager(mgr)

	state, derr := m.GetUnitFileState("hello.service")
	if derr != nil {
		t.Fatalf("GetUnitFileState error: %v", derr)
	}
	if state != "disabled" && state != "enabled" {
		t.Fatalf("state=%q", state)
	}

	// Enable and re-query.
	if err := mgr.EnableUnit("hello.service"); err != nil {
		t.Fatalf("EnableUnit: %v", err)
	}
	state2, _ := m.GetUnitFileState("hello.service")
	if state2 != "enabled" {
		t.Fatalf("after enable state=%q, want enabled", state2)
	}
}

func TestBuildManagerProps(t *testing.T) {
	mgr := newTestManager(t)
	props := buildManagerProps(mgr)
	mgrProps, ok := props[managerInterface]
	if !ok {
		t.Fatalf("missing interface %s", managerInterface)
	}
	if _, ok := mgrProps["Version"]; !ok {
		t.Fatalf("missing Version")
	}
	if _, ok := mgrProps["SystemState"]; !ok {
		t.Fatalf("missing SystemState")
	}
	// Ensure no duplicate keys (would have panicked before, but sanity check).
	if len(mgrProps) < 5 {
		t.Fatalf("too few manager props: %d", len(mgrProps))
	}
}
