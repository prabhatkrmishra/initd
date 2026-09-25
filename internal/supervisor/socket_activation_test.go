package supervisor

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// What a socket-activated daemon is told about its fds. sd_listen_fds() answers
// nothing to a process whose pid does not match $LISTEN_PID, and
// sd_listen_fds_with_names() hands back one name per fd, so both have to be
// right in the child's own environment - which is read back out of /proc here
// rather than from whatever the supervisor meant to pass.
func TestSocketActivationEnvironmentMatchesSystemd(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := filepath.Join(dir, "env.sock")
	writeUnit(t, dir, "env.socket", "[Socket]\nListenStream="+sockPath+"\n")
	writeUnit(t, dir, "env.service", "[Service]\nExecStart=/bin/sleep 30\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	if err := m.StartUnit("env.socket"); err != nil {
		t.Fatalf("start socket: %v", err)
	}
	defer m.StopUnit("env.socket")
	defer m.StopUnit("env.service")

	time.Sleep(200 * time.Millisecond)
	c, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if u, err := m.FindUnit("env.service"); err == nil {
			if snap := u.Snapshot(); snap.State == "active" {
				pid = snap.MainPID
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pid <= 0 {
		t.Fatal("the service never came up off the connection")
	}

	env := procEnviron(t, pid)
	if got := env["LISTEN_PID"]; got != strconv.Itoa(pid) {
		t.Errorf("child LISTEN_PID=%q, want its own pid %d (sd_listen_fds rejects anything else)", got, pid)
	}
	if got := env["LISTEN_FDS"]; got != "1" {
		t.Errorf("child LISTEN_FDS=%q, want 1", got)
	}
	if got := env["LISTEN_FDNAMES"]; got != "env.socket" {
		t.Errorf("child LISTEN_FDNAMES=%q, want the socket unit's name", got)
	}
}

func procEnviron(t *testing.T, pid int) map[string]string {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		t.Skipf("cannot read the child's environment: %v", err)
	}
	out := map[string]string{}
	for _, kv := range strings.Split(string(data), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// Cold-starting a socket-activated service must not eat the connection that
// triggered it: the parent polls for readability and never Accept()s, so the
// first request stays queued for the child via LISTEN_FDS.
func TestSocketActivationKeepsTrigger(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := filepath.Join(dir, "trigger.sock")
	writeUnit(t, dir, "trigger.socket", "[Socket]\nListenStream="+sockPath+"\n")
	writeUnit(t, dir, "trigger.service", "[Service]\nExecStart=/bin/sleep 30\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	if err := m.StartUnit("trigger.socket"); err != nil {
		t.Fatalf("start socket: %v", err)
	}
	defer m.StopUnit("trigger.socket")
	defer m.StopUnit("trigger.service")

	// Give the poll loop a moment to start.
	time.Sleep(200 * time.Millisecond)

	c, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// The service should cold-start off this connection.
	deadline := time.Now().Add(5 * time.Second)
	started := false
	for time.Now().Before(deadline) {
		if u, err := m.FindUnit("trigger.service"); err == nil {
			if snap := u.Snapshot(); snap.State == "active" || snap.State == "activating" {
				started = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !started {
		t.Fatalf("service did not activate off socket trigger")
	}

	// The triggering connection must still be open (no EOF/reset). The
	// child (sleep) never accepts, so a Read blocks until deadline.
	_ = c.SetReadDeadline(time.Now().Add(600 * time.Millisecond))
	buf := make([]byte, 1)
	_, err = c.Read(buf)
	if err == io.EOF {
		t.Fatalf("triggering connection was consumed/closed by parent (EOF); first request eaten")
	}
	// Timeout (or i/o timeout) is expected: connection queued, not closed.
	if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("expected read timeout on queued connection, got %v", err)
	}
}
