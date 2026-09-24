package supervisor

import (
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

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
