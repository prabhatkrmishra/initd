package service

import (
	"os/exec"
	"testing"
	"time"
)

func TestExecStartBinaries(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/usr/sbin/sshd -D", "sshd"},
		{"/usr/sbin/mydaemon --state=/var/lib/mydaemon/state", "mydaemon"},
		{"@-/usr/sbin/sshd -D", "sshd"},
		{"-/usr/bin/foo bar", "foo"},
		{"FOO=1 /usr/bin/bar --x", "bar"},
		{"", ""},
	}
	for _, c := range cases {
		got := execStartBinaries(c.in)
		if c.want == "" {
			if len(got) != 0 {
				t.Fatalf("execStartBinaries(%q) = %v, want empty", c.in, got)
			}
			continue
		}
		if len(got) == 0 || got[0] != c.want {
			t.Fatalf("execStartBinaries(%q) = %v, want basename %q", c.in, got, c.want)
		}
	}
}

// Host-verified hazard: the argv scan also matches the process this daemon
// just killed (still in /proc) and any second unit sharing a byte-identical
// ExecStart. `systemctl stop X && systemctl is-active X` answering active
// made every restart script that gates on the answer skip the start, so a
// deliberately stopped unit must stay stopped.
func TestEffectiveStateStoppedStaysInactive(t *testing.T) {
	const arg = "317"
	u := newTestUnit(t, "zzstopped.service", "[Service]\nType=simple\nExecStart=/bin/sleep "+arg+"\n")
	if _, err := u.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForState(t, u, StateActive)
	if err := u.Stop(3 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	ext := exec.Command("sleep", arg)
	if err := ext.Start(); err != nil {
		t.Skip("sleep unavailable")
	}
	defer ext.Process.Kill()
	if pid, _ := u.FindExternalPID(); pid != ext.Process.Pid {
		t.Fatalf("setup: the scan should match the external sleep, got %d", pid)
	}

	if state, pid := u.EffectiveState(); state != StateInactive || pid != 0 {
		t.Errorf("stopped unit reports %s (pid %d), want inactive", state, pid)
	}

	// The compatibility path itself must keep working for a unit this
	// daemon never started or stopped.
	u2 := newTestUnit(t, "zznever.service", "[Service]\nType=simple\nExecStart=/bin/sleep "+arg+"\n")
	if state, pid := u2.EffectiveState(); state != StateActive || pid != ext.Process.Pid {
		t.Errorf("externally started unit reports %s (pid %d), want active with the external pid", state, pid)
	}
}
