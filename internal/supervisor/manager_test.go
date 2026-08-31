package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"initd/internal/parser"
	"initd/internal/service"
)

// newTestManager creates a manager whose search paths and enabled root point
// at temp directories, so tests never touch the real system.
func newTestManager(t *testing.T) (*Manager, string) {
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
	m := NewSystemManager()
	m.SearchPaths = []string{searchDir}
	m.EnabledRoot = enabledRoot
	// Run in user mode so LoadUnits skips tmpfiles setup (which needs root).
	m.UserMode = true
	return m, searchDir
}

func writeUnit(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestParseKillSignal(t *testing.T) {
	cases := []struct {
		in   string
		want syscall.Signal
	}{
		{"", syscall.SIGTERM},
		{"9", syscall.SIGKILL},
		{"15", syscall.SIGTERM},
		{"-9", syscall.SIGKILL},
		{"KILL", syscall.SIGKILL},
		{"SIGKILL", syscall.SIGKILL},
		{"HUP", syscall.SIGHUP},
		{"SIGHUP", syscall.SIGHUP},
		{"bogus", syscall.SIGTERM},
	}
	for _, c := range cases {
		if got := parseKillSignal(c.in); got != c.want {
			t.Errorf("parseKillSignal(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestOrderUnitsByAfter(t *testing.T) {
	m, _ := newTestManager(t)
	mk := func(name, after string) *service.Unit {
		cfg := &parser.Unit{Name: name, Type: "service"}
		if after != "" {
			cfg.After = strings.Fields(after)
		}
		return service.NewUnit(cfg, name)
	}

	a := mk("a.service", "")
	b := mk("b.service", "a.service")
	c := mk("c.service", "b.service")

	ordered := m.orderUnitsByAfter([]*service.Unit{c, a, b})
	pos := map[string]int{}
	for i, u := range ordered {
		pos[u.Config.Name] = i
	}
	if pos["a.service"] > pos["b.service"] {
		t.Errorf("a.service should come before b.service, got %v", pos)
	}
	if pos["b.service"] > pos["c.service"] {
		t.Errorf("b.service should come before c.service, got %v", pos)
	}
}

func TestOrderUnitsByAfterCycle(t *testing.T) {
	m, _ := newTestManager(t)
	mk := func(name, after string) *service.Unit {
		cfg := &parser.Unit{Name: name, Type: "service"}
		if after != "" {
			cfg.After = strings.Fields(after)
		}
		return service.NewUnit(cfg, name)
	}
	a := mk("a.service", "b.service")
	b := mk("b.service", "a.service")
	// Cycle should not hang; falls back to file order.
	ordered := m.orderUnitsByAfter([]*service.Unit{a, b})
	if len(ordered) != 2 {
		t.Fatalf("expected 2 units, got %d", len(ordered))
	}
}

func TestUnitStateAndIsFailed(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "ok.service", "[Service]\nExecStart=/bin/true\n")
	writeUnit(t, dir, "bad.service", "[Service]\nExecStart=/bin/true\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}

	// Fresh units are inactive, not failed.
	state, err := m.UnitState("ok.service")
	if err != nil {
		t.Fatalf("UnitState: %v", err)
	}
	if state != "inactive" {
		t.Errorf("UnitState(ok) = %q, want inactive", state)
	}

	// Mark one failed.
	bad, err := m.FindUnit("bad.service")
	if err != nil {
		t.Fatalf("FindUnit: %v", err)
	}
	bad.MarkFailed("test failure")

	if failed, _ := m.IsFailed("bad.service"); !failed {
		t.Error("IsFailed(bad) should be true")
	}
	if failed, _ := m.IsFailed("ok.service"); failed {
		t.Error("IsFailed(ok) should be false")
	}
	// Empty name: any failed unit -> true
	if failed, _ := m.IsFailed(""); !failed {
		t.Error("IsFailed(\"\") should be true when a unit failed")
	}
	// UnitState with empty name reports failed
	if state, _ := m.UnitState(""); state != "failed" {
		t.Errorf("UnitState(\"\") = %q, want failed", state)
	}

	// ResetFailed clears it.
	if err := m.ResetFailed("bad.service"); err != nil {
		t.Fatalf("ResetFailed: %v", err)
	}
	if failed, _ := m.IsFailed("bad.service"); failed {
		t.Error("IsFailed(bad) should be false after reset")
	}
	if state, _ := m.UnitState(""); state != "running" {
		t.Errorf("UnitState(\"\") after reset = %q, want running", state)
	}

	// Unknown unit.
	if _, err := m.UnitState("nope.service"); err == nil {
		t.Error("UnitState(unknown) should error")
	}
}

func TestSystemState(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "ok.service", "[Service]\nExecStart=/bin/true\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	if got := m.SystemState(); got != "running" {
		t.Errorf("SystemState = %q, want running", got)
	}
	u, _ := m.FindUnit("ok.service")
	u.MarkFailed("boom")
	if got := m.SystemState(); got != "degraded" {
		t.Errorf("SystemState after failure = %q, want degraded", got)
	}
}

func TestMaskUnmask(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "foo.service", "[Service]\nExecStart=/bin/true\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	if m.IsMasked("foo.service") {
		t.Error("should not be masked initially")
	}
	if err := m.MaskUnit("foo.service"); err != nil {
		t.Fatalf("MaskUnit: %v", err)
	}
	if !m.IsMasked("foo.service") {
		t.Error("should be masked after MaskUnit")
	}
	if err := m.UnmaskUnit("foo.service"); err != nil {
		t.Fatalf("UnmaskUnit: %v", err)
	}
	if m.IsMasked("foo.service") {
		t.Error("should not be masked after UnmaskUnit")
	}
	// Masking an unknown unit errors.
	if err := m.MaskUnit("nope.service"); err == nil {
		t.Error("MaskUnit(unknown) should error")
	}
}

func TestEnableDisableIsEnabled(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "foo.service", `
[Service]
ExecStart=/bin/true

[Install]
WantedBy=multi-user.target
`)
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}

	enabled, err := m.IsEnabled("foo.service")
	if err != nil {
		t.Fatalf("IsEnabled: %v", err)
	}
	if enabled {
		t.Error("should be disabled initially")
	}
	if got := m.UnitFileState("foo.service"); got != "disabled" {
		t.Errorf("UnitFileState = %q, want disabled", got)
	}

	if err := m.EnableUnit("foo.service"); err != nil {
		t.Fatalf("EnableUnit: %v", err)
	}
	enabled, _ = m.IsEnabled("foo.service")
	if !enabled {
		t.Error("should be enabled after EnableUnit")
	}
	if got := m.UnitFileState("foo.service"); got != "enabled" {
		t.Errorf("UnitFileState = %q, want enabled", got)
	}

	if err := m.DisableUnit("foo.service"); err != nil {
		t.Fatalf("DisableUnit: %v", err)
	}
	enabled, _ = m.IsEnabled("foo.service")
	if enabled {
		t.Error("should be disabled after DisableUnit")
	}
}

func TestEnableUnitNoWantedBy(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "foo.service", "[Service]\nExecStart=/bin/true\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	if err := m.EnableUnit("foo.service"); err == nil {
		t.Error("EnableUnit without WantedBy should error")
	}
}

func TestShowAndCatUnit(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "foo.service", `
[Unit]
Description=Show me

[Service]
ExecStart=/bin/true
`)
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	data, err := m.ShowUnit("foo.service")
	if err != nil {
		t.Fatalf("ShowUnit: %v", err)
	}
	if data["Id"] != "foo.service" {
		t.Errorf("ShowUnit Id = %q", data["Id"])
	}
	if data["Description"] != "Show me" {
		t.Errorf("ShowUnit Description = %q", data["Description"])
	}
	if data["ActiveState"] != "inactive" {
		t.Errorf("ShowUnit ActiveState = %q", data["ActiveState"])
	}

	content, err := m.CatUnit("foo.service")
	if err != nil {
		t.Fatalf("CatUnit: %v", err)
	}
	if content == "" {
		t.Error("CatUnit returned empty content")
	}
}

func TestListUnitFiles(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "foo.service", "[Service]\nExecStart=/bin/true\n")
	writeUnit(t, dir, "bar.socket", "[Socket]\nListenStream=/tmp/bar.sock\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	units, err := m.ListUnitFiles()
	if err != nil {
		t.Fatalf("ListUnitFiles: %v", err)
	}
	names := map[string]bool{}
	for _, u := range units {
		names[u.Config.Name] = true
	}
	if !names["foo.service"] {
		t.Error("ListUnitFiles missing foo.service")
	}
	if !names["bar.socket"] {
		t.Error("ListUnitFiles missing bar.socket")
	}
}

func TestSocketStartStopAndState(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := filepath.Join(dir, "test.sock")
	writeUnit(t, dir, "foo.socket", "[Socket]\nListenStream="+sockPath+"\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}

	state, err := m.SocketActiveState("foo.socket")
	if err != nil {
		t.Fatalf("SocketActiveState: %v", err)
	}
	if state != "inactive" {
		t.Errorf("SocketActiveState = %q, want inactive", state)
	}

	if err := m.StartUnit("foo.socket"); err != nil {
		t.Fatalf("StartUnit(socket): %v", err)
	}
	state, _ = m.SocketActiveState("foo.socket")
	if state != "active" {
		t.Errorf("SocketActiveState after start = %q, want active", state)
	}
	if _, err := os.Stat(sockPath); err != nil {
		t.Errorf("socket file should exist: %v", err)
	}

	if err := m.StopUnit("foo.socket"); err != nil {
		t.Fatalf("StopUnit(socket): %v", err)
	}
	state, _ = m.SocketActiveState("foo.socket")
	if state != "inactive" {
		t.Errorf("SocketActiveState after stop = %q, want inactive", state)
	}
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("socket file should be removed after stop, got %v", err)
	}
}

func TestShowAndCatSocketUnit(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := filepath.Join(dir, "test.sock")
	writeUnit(t, dir, "foo.socket", "[Socket]\nListenStream="+sockPath+"\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	data, err := m.ShowSocketUnit("foo.socket")
	if err != nil {
		t.Fatalf("ShowSocketUnit: %v", err)
	}
	if data["Type"] != "socket" {
		t.Errorf("ShowSocketUnit Type = %q", data["Type"])
	}
	if data["ListenStream"] != sockPath {
		t.Errorf("ShowSocketUnit ListenStream = %q", data["ListenStream"])
	}
	content, err := m.CatSocketUnit("foo.socket")
	if err != nil {
		t.Fatalf("CatSocketUnit: %v", err)
	}
	if content == "" {
		t.Error("CatSocketUnit returned empty content")
	}
}

// TestSocketDependencyResolution verifies the Phase 2 fix: a service that
// Requires= a .socket unit starts the socket before the service.
func TestSocketDependencyResolution(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := filepath.Join(dir, "dep.sock")
	writeUnit(t, dir, "dep.socket", "[Socket]\nListenStream="+sockPath+"\n")
	writeUnit(t, dir, "app.service", `
[Unit]
Requires=dep.socket

[Service]
Type=simple
ExecStart=/bin/sleep 30
`)
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}

	if err := m.StartUnit("app.service"); err != nil {
		t.Fatalf("StartUnit(app): %v", err)
	}

	// The socket dependency should have been started.
	state, err := m.SocketActiveState("dep.socket")
	if err != nil {
		t.Fatalf("SocketActiveState: %v", err)
	}
	if state != "active" {
		t.Errorf("dep.socket state = %q, want active (dependency should start it)", state)
	}

	// Clean up.
	_ = m.StopUnit("app.service")
	_ = m.StopUnit("dep.socket")
}

// TestRestartOnStop verifies the Phase 3 fix: a Restart=always service must
// not restart after a manual stop.
func TestRestartOnStop(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "loop.service", `
[Service]
Type=simple
ExecStart=/bin/sleep 30
Restart=always
RestartSec=0
`)
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}

	if err := m.StartUnit("loop.service"); err != nil {
		t.Fatalf("StartUnit: %v", err)
	}
	u, _ := m.FindUnit("loop.service")
	// Wait for active.
	waitForState(t, u, service.StateActive)

	if err := m.StopUnit("loop.service"); err != nil {
		t.Fatalf("StopUnit: %v", err)
	}

	// Give the restart watcher time to (incorrectly) restart if it were going to.
	time.Sleep(1500 * time.Millisecond)

	state := u.Snapshot().State
	if state == service.StateActive || state == service.StateActivating {
		t.Errorf("service restarted after manual stop, state=%s", state)
	}
	if state != service.StateInactive {
		t.Errorf("state after stop = %s, want inactive", state)
	}
}

// TestNotifyCleanStop verifies the Phase 4 fix: stopping a Type=notify
// service must not mark it failed, even after the readiness timer fires.
func TestNotifyCleanStop(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "notify.service", `
[Service]
Type=notify
ExecStart=/bin/sleep 30
TimeoutStartSec=1
`)
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}

	if err := m.StartUnit("notify.service"); err != nil {
		t.Fatalf("StartUnit: %v", err)
	}
	u, _ := m.FindUnit("notify.service")

	// The service never sends READY=1, so it stays activating until the
	// readiness timeout. Stop it while activating.
	waitForState(t, u, service.StateActivating)

	if err := m.StopUnit("notify.service"); err != nil {
		t.Fatalf("StopUnit: %v", err)
	}

	// Wait past the readiness timeout so the notify timer path runs too.
	time.Sleep(1500 * time.Millisecond)

	state := u.Snapshot().State
	if state == service.StateFailed {
		t.Errorf("notify service marked failed after clean stop: %+v", u.Snapshot())
	}
	if state != service.StateInactive {
		t.Errorf("notify service state after stop = %s, want inactive", state)
	}
}

func waitForState(t *testing.T, u *service.Unit, want service.State) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if u.Snapshot().State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("unit did not reach state %s, current=%s", want, u.Snapshot().State)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
