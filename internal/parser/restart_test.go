package parser

import "testing"

func TestParseRestartPolicy(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "flap.service", `
[Unit]
Description=Flappy
StartLimitIntervalSec=30
StartLimitBurst=3

[Service]
ExecStart=/bin/false
Restart=on-failure
RestartSec=2s
RestartSteps=3
RestartMaxDelaySec=20s
`)
	unit, err := ParseUnit(path)
	if err != nil {
		t.Fatalf("ParseUnit: %v", err)
	}
	if unit.StartLimitIntervalSec != "30" || unit.StartLimitBurst != "3" {
		t.Fatalf("start limit = %q/%q", unit.StartLimitIntervalSec, unit.StartLimitBurst)
	}
	if unit.Service.RestartSteps != "3" || unit.Service.RestartMaxDelaySec != "20s" {
		t.Fatalf("restart backoff = %q/%q", unit.Service.RestartSteps, unit.Service.RestartMaxDelaySec)
	}

	drop := writeFile(t, dir, "flap.service.d/override.conf", `
[Unit]
StartLimitBurst=7

[Service]
RestartSec=5s
`)
	_ = drop
	merged, err := ParseUnitWithDropins(path, []string{dir}, "")
	if err != nil {
		t.Fatalf("ParseUnitWithDropins: %v", err)
	}
	if merged.StartLimitBurst != "7" {
		t.Fatalf("drop-in burst = %q, want 7", merged.StartLimitBurst)
	}
	if merged.Service.RestartSec != "5s" {
		t.Fatalf("drop-in restartsec = %q, want 5s", merged.Service.RestartSec)
	}
	if merged.StartLimitIntervalSec != "30" {
		t.Fatalf("interval should survive merge, got %q", merged.StartLimitIntervalSec)
	}
}
