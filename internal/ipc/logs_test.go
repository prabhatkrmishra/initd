package ipc

import (
	"testing"

	"initd/internal/logging"
)

func TestDispatchLogs(t *testing.T) {
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
	u, err := m.FindUnit("app.service")
	if err != nil {
		t.Fatalf("FindUnit: %v", err)
	}
	u.Log(logging.LevelInfo, "first")
	u.Log(logging.LevelInfo, "second")
	u.Log(logging.LevelError, "third")

	resp := dispatch(Request{Action: "logs", Unit: "app.service"}, m)
	if !resp.Success {
		t.Fatalf("logs failed: %v", resp.Message)
	}
	lines, ok := resp.Data.([]string)
	if !ok {
		t.Fatalf("logs Data type = %T, want []string", resp.Data)
	}
	if len(lines) != 3 {
		t.Fatalf("logs lines = %d, want 3: %v", len(lines), lines)
	}

	resp = dispatch(Request{Action: "logs", Unit: "app.service", Lines: 2}, m)
	if !resp.Success {
		t.Fatalf("logs -n failed: %v", resp.Message)
	}
	lines, ok = resp.Data.([]string)
	if !ok || len(lines) != 2 {
		t.Fatalf("logs -n Data = %v, want 2 lines", resp.Data)
	}

	resp = dispatch(Request{Action: "logs", Unit: "missing.service"}, m)
	if resp.Success {
		t.Error("logs for missing unit should fail")
	}

	resp = dispatch(Request{Action: "logs"}, m)
	if resp.Success {
		t.Error("logs without unit should fail")
	}
}
