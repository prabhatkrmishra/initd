package service

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A Type=forking daemon that writes no pid file is still a running service:
// upstream takes the main PID from the unit cgroup, and without cgroups the
// surviving member of the starter's process group is the only structural
// answer. Requiring a PIDFile turned forking services that do not ship one
// into permanent start failures.
func TestForkingAdoptsDaemonWithoutPIDFile(t *testing.T) {
	u := newTestUnit(t, "zzfork.service", "[Service]\nType=forking\nTimeoutStartSec=5\nExecStart=/bin/sh -c '( sleep 120 & ) ; exit 0'\n")
	defer func() { _ = u.Stop(2 * time.Second) }()

	if _, err := u.StartAndWait(true); err != nil {
		t.Fatalf("StartAndWait: %v", err)
	}
	waitForState(t, u, StateActive)
	snap := u.Snapshot()
	if snap.MainPID <= 0 {
		t.Fatalf("MainPID = %d, want the forked daemon adopted", snap.MainPID)
	}
	if !processAlive(snap.MainPID) {
		t.Errorf("adopted MainPID %d is not alive", snap.MainPID)
	}
}

// With a PIDFile the daemon it names wins over anything else in the group:
// SysV scripts on the target box daemonise out of the supervisor's process
// group and only the pid file identifies them.
func TestForkingUsesPIDFile(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "daemon.pid")
	u := newTestUnit(t, "zzforkpid.service", fmt.Sprintf(
		"[Service]\nType=forking\nTimeoutStartSec=5\nPIDFile=%s\nExecStart=/bin/sh -c 'sleep 120 & echo $! > %s; exit 0'\n",
		pidPath, pidPath))
	defer func() { _ = u.Stop(2 * time.Second) }()

	if _, err := u.StartAndWait(true); err != nil {
		t.Fatalf("StartAndWait: %v", err)
	}
	waitForState(t, u, StateActive)
	snap := u.Snapshot()
	raw, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	var want int
	if _, serr := fmt.Sscanf(string(raw), "%d", &want); serr != nil {
		t.Fatalf("parse pid file %q: %v", raw, serr)
	}
	if snap.MainPID != want {
		t.Errorf("MainPID = %d, want the pid file's %d", snap.MainPID, want)
	}
}
