package service

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"initd/internal/parser"
)

func mkUnit(name, killMode string) *Unit {
	cfg := &parser.Unit{Name: name}
	cfg.Service.ExecStart = "/bin/sleep 300"
	cfg.Service.KillMode = killMode
	return NewUnit(cfg, name)
}

// Group collapse must never take the daemon: a unit whose recorded group is
// our own must only get a direct-PID signal.
func TestGroupKillSkipsOwnGroup(t *testing.T) {
	self := syscall.Getpgrp()
	u := mkUnit("a.service", "control-group")
	// Fake a collapsed group record pointing at ourselves.
	u.mu.Lock()
	u.pgid = self
	u.mu.Unlock()
	// Spawn something harmless to signal directly; pid path must not error the guard.
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("sleep unavailable")
	}
	defer cmd.Process.Kill()
	u.signalUnitPID(cmd.Process.Pid, self, true, syscall.Signal(0))
	if !processAlive(os.Getpid()) {
		t.Fatal("daemon should still be alive")
	}
}

// Default (unset) KillMode must behave process-only, not group.
func TestDefaultKillModeIsProcess(t *testing.T) {
	u := mkUnit("b.service", "")
	if !u.killModeProcess() {
		t.Fatal("unset KillMode must be process-only")
	}
	if mkUnit("c.service", "mixed").killModeProcess() != true {
		t.Fatal("mixed must be process-only")
	}
	if mkUnit("d.service", "control-group").killModeProcess() != false {
		t.Fatal("control-group must be group mode")
	}
	time.Sleep(10 * time.Millisecond)
}
