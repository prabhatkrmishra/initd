package ipc

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"initd/internal/service"
	"initd/internal/supervisor"
)

// newTestManager builds a manager with temp search/enabled dirs so dispatch
// tests never touch the real system.
func newTestManager(t *testing.T) (*supervisor.Manager, string) {
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

func waitForState(t *testing.T, m *supervisor.Manager, name string, want service.State) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		u, err := m.FindUnit(name)
		if err == nil && u.Snapshot().State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("unit %s did not reach state %s", name, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDispatchUnknownAction(t *testing.T) {
	m, _ := newTestManager(t)
	resp := dispatch(Request{Action: "frobnicate"}, m)
	if resp.Success {
		t.Error("unknown action should fail")
	}
}

func TestDispatchStartStopStatus(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "app.service", `
[Unit]
Description=App

[Service]
Type=simple
ExecStart=/bin/sleep 30
`)
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}

	// start
	resp := dispatch(Request{Action: "start", Unit: "app.service"}, m)
	if !resp.Success {
		t.Fatalf("start failed: %v", resp.Message)
	}
	waitForState(t, m, "app.service", service.StateActive)

	// status
	resp = dispatch(Request{Action: "status", Unit: "app.service"}, m)
	if !resp.Success {
		t.Fatalf("status failed: %v", resp.Message)
	}
	data, ok := resp.Data.(StatusData)
	if !ok {
		t.Fatalf("status Data type = %T, want StatusData", resp.Data)
	}
	if data.Name != "app.service" {
		t.Errorf("status Name = %q", data.Name)
	}
	if data.Description != "App" {
		t.Errorf("status Description = %q", data.Description)
	}
	if data.State != service.StateActive {
		t.Errorf("status State = %s, want active", data.State)
	}

	// is-active
	resp = dispatch(Request{Action: "is-active", Unit: "app.service"}, m)
	if !resp.Success || resp.Data != service.StateActive {
		t.Errorf("is-active = %v, %v; want active", resp.Data, resp.Message)
	}

	// stop
	resp = dispatch(Request{Action: "stop", Unit: "app.service"}, m)
	if !resp.Success {
		t.Fatalf("stop failed: %v", resp.Message)
	}
	resp = dispatch(Request{Action: "is-active", Unit: "app.service"}, m)
	if !resp.Success || resp.Data != service.StateInactive {
		t.Errorf("is-active after stop = %v, %v; want inactive", resp.Data, resp.Message)
	}
}

func TestDispatchRestart(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "app.service", "[Service]\nType=simple\nExecStart=/bin/sleep 30\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	if resp := dispatch(Request{Action: "start", Unit: "app.service"}, m); !resp.Success {
		t.Fatalf("start failed: %v", resp.Message)
	}
	waitForState(t, m, "app.service", service.StateActive)
	if resp := dispatch(Request{Action: "restart", Unit: "app.service"}, m); !resp.Success {
		t.Fatalf("restart failed: %v", resp.Message)
	}
	// After restart it should be active again.
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp := dispatch(Request{Action: "is-active", Unit: "app.service"}, m)
		if resp.Success && resp.Data == service.StateActive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("service not active after restart")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = dispatch(Request{Action: "stop", Unit: "app.service"}, m)
}

func TestDispatchListUnits(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "app.service", "[Service]\nExecStart=/bin/true\n")
	writeUnit(t, dir, "sock.socket", "[Socket]\nListenStream="+filepath.Join(dir, "s.sock")+"\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	resp := dispatch(Request{Action: "list-units"}, m)
	if !resp.Success {
		t.Fatalf("list-units failed: %v", resp.Message)
	}
	data, ok := resp.Data.([]UnitData)
	if !ok {
		t.Fatalf("list-units Data type = %T", resp.Data)
	}
	names := map[string]bool{}
	for _, u := range data {
		names[u.Name] = true
	}
	if !names["app.service"] {
		t.Error("list-units missing app.service")
	}
	if !names["sock.socket"] {
		t.Error("list-units missing sock.socket")
	}
}

func TestDispatchListUnitFiles(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "app.service", "[Service]\nExecStart=/bin/true\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	resp := dispatch(Request{Action: "list-unit-files"}, m)
	if !resp.Success {
		t.Fatalf("list-unit-files failed: %v", resp.Message)
	}
	data, ok := resp.Data.([]UnitFileData)
	if !ok {
		t.Fatalf("list-unit-files Data type = %T", resp.Data)
	}
	if len(data) == 0 {
		t.Fatal("list-unit-files returned no units")
	}
	if data[0].Name != "app.service" {
		t.Errorf("list-unit-files first = %q", data[0].Name)
	}
	if data[0].State != "disabled" {
		t.Errorf("list-unit-files state = %q, want disabled", data[0].State)
	}
}

func TestDispatchEnableDisableIsEnabled(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "app.service", `
[Service]
ExecStart=/bin/true

[Install]
WantedBy=multi-user.target
`)
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}

	resp := dispatch(Request{Action: "is-enabled", Unit: "app.service"}, m)
	if !resp.Success || resp.Data != "disabled" {
		t.Errorf("is-enabled = %v, %v; want disabled", resp.Data, resp.Message)
	}

	if resp := dispatch(Request{Action: "enable", Unit: "app.service"}, m); !resp.Success {
		t.Fatalf("enable failed: %v", resp.Message)
	}
	resp = dispatch(Request{Action: "is-enabled", Unit: "app.service"}, m)
	if !resp.Success || resp.Data != "enabled" {
		t.Errorf("is-enabled after enable = %v, %v; want enabled", resp.Data, resp.Message)
	}

	if resp := dispatch(Request{Action: "disable", Unit: "app.service"}, m); !resp.Success {
		t.Fatalf("disable failed: %v", resp.Message)
	}
	resp = dispatch(Request{Action: "is-enabled", Unit: "app.service"}, m)
	if !resp.Success || resp.Data != "disabled" {
		t.Errorf("is-enabled after disable = %v, %v; want disabled", resp.Data, resp.Message)
	}
}

func TestDispatchIsFailedAndResetFailed(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "app.service", "[Service]\nExecStart=/bin/true\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}

	// Not failed initially.
	resp := dispatch(Request{Action: "is-failed", Unit: "app.service"}, m)
	if !resp.Success || resp.Data != "inactive" {
		t.Errorf("is-failed = %v, %v; want inactive", resp.Data, resp.Message)
	}

	// Mark failed directly.
	u, _ := m.FindUnit("app.service")
	u.MarkFailed("boom")
	resp = dispatch(Request{Action: "is-failed", Unit: "app.service"}, m)
	if !resp.Success || resp.Data != "failed" {
		t.Errorf("is-failed after failure = %v, %v; want failed", resp.Data, resp.Message)
	}

	// reset-failed
	if resp := dispatch(Request{Action: "reset-failed", Unit: "app.service"}, m); !resp.Success {
		t.Fatalf("reset-failed failed: %v", resp.Message)
	}
	resp = dispatch(Request{Action: "is-failed", Unit: "app.service"}, m)
	if !resp.Success || resp.Data != "inactive" {
		t.Errorf("is-failed after reset = %v, %v; want inactive", resp.Data, resp.Message)
	}
}

func TestDispatchShowAndCat(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "app.service", `
[Unit]
Description=Show me

[Service]
ExecStart=/bin/true
`)
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}

	resp := dispatch(Request{Action: "show", Unit: "app.service"}, m)
	if !resp.Success {
		t.Fatalf("show failed: %v", resp.Message)
	}
	data, ok := resp.Data.(map[string]string)
	if !ok {
		t.Fatalf("show Data type = %T", resp.Data)
	}
	if data["Description"] != "Show me" {
		t.Errorf("show Description = %q", data["Description"])
	}

	resp = dispatch(Request{Action: "cat", Unit: "app.service"}, m)
	if !resp.Success {
		t.Fatalf("cat failed: %v", resp.Message)
	}
	if content, ok := resp.Data.(string); !ok || content == "" {
		t.Errorf("cat Data = %v, want non-empty string", resp.Data)
	}
}

func TestDispatchMaskUnmask(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "app.service", "[Service]\nExecStart=/bin/true\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}

	if resp := dispatch(Request{Action: "mask", Unit: "app.service"}, m); !resp.Success {
		t.Fatalf("mask failed: %v", resp.Message)
	}
	resp := dispatch(Request{Action: "is-enabled", Unit: "app.service"}, m)
	if !resp.Success || resp.Data != "masked" {
		t.Errorf("is-enabled after mask = %v, %v; want masked", resp.Data, resp.Message)
	}
	if resp := dispatch(Request{Action: "unmask", Unit: "app.service"}, m); !resp.Success {
		t.Fatalf("unmask failed: %v", resp.Message)
	}
}

func TestDispatchKill(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "app.service", "[Service]\nType=simple\nExecStart=/bin/sleep 30\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	if resp := dispatch(Request{Action: "start", Unit: "app.service"}, m); !resp.Success {
		t.Fatalf("start failed: %v", resp.Message)
	}
	waitForState(t, m, "app.service", service.StateActive)
	// Kill the active unit.
	if resp := dispatch(Request{Action: "kill", Unit: "app.service", Signal: "TERM"}, m); !resp.Success {
		t.Errorf("kill failed: %v", resp.Message)
	}
	_ = dispatch(Request{Action: "stop", Unit: "app.service"}, m)
}

func TestDispatchIsSystemRunning(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "app.service", "[Service]\nExecStart=/bin/true\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	resp := dispatch(Request{Action: "is-system-running"}, m)
	if !resp.Success || resp.Data != "running" {
		t.Errorf("is-system-running = %v, %v; want running", resp.Data, resp.Message)
	}
}

func TestDispatchDaemonReload(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "app.service", "[Service]\nExecStart=/bin/true\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	if resp := dispatch(Request{Action: "daemon-reload"}, m); !resp.Success {
		t.Fatalf("daemon-reload failed: %v", resp.Message)
	}
}

func TestDispatchRebootUserModeDenied(t *testing.T) {
	m, _ := newTestManager(t)
	// UserMode is true in the test manager, so reboot must be denied.
	resp := dispatch(Request{Action: "reboot"}, m)
	if resp.Success {
		t.Error("reboot should be denied for user manager")
	}
}

func TestDispatchStatusNotFound(t *testing.T) {
	m, _ := newTestManager(t)
	resp := dispatch(Request{Action: "status", Unit: "nope.service"}, m)
	if resp.Success {
		t.Error("status for unknown unit should fail")
	}
}
