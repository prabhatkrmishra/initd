package notify

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStartUsesFilesystemSocket(t *testing.T) {
	s, err := Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	if s.Path == "" || filepath.IsAbs(s.Path) == false {
		t.Fatalf("Path = %q, want an absolute filesystem path", s.Path)
	}
	fi, err := os.Stat(s.Path)
	if err != nil {
		t.Fatalf("stat notify socket: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o666 {
		t.Errorf("socket mode = %o, want 666", got)
	}

	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: s.Path, Net: "unixgram"})
	if err != nil {
		t.Fatalf("dial notify socket: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("READY=1")); err != nil {
		t.Fatalf("send READY: %v", err)
	}
	select {
	case <-s.Ready:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not observe READY=1")
	}

	s.Stop()
	if _, err := os.Stat(s.Path); !os.IsNotExist(err) {
		t.Errorf("socket file still exists after Stop: %v", err)
	}
}

func TestSweepStaleKeepsLiveAndRemovesDead(t *testing.T) {
	dir := Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Skipf("cannot create notify dir: %v", err)
	}

	live := filepath.Join(dir, fmt.Sprintf("notify-%d-900001.sock", os.Getpid()))
	dead := filepath.Join(dir, "notify-99999999-900002.sock")
	for _, path := range []string{live, dead} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("create %s: %v", path, err)
		}
	}
	defer os.Remove(live)

	SweepStale()

	if _, err := os.Stat(live); err != nil {
		t.Errorf("live manager's socket was removed: %v", err)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Errorf("stale socket still exists: %v", err)
	}
}

// receiveOneDatagram drains a single datagram from conn, tolerating nothing
// else on the wire.
func receiveOneDatagram(t *testing.T, conn *net.UnixConn) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read notify datagram: %v", err)
	}
	return string(buf[:n])
}

func TestReadySendsREADY1ToFilesystemSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()

	t.Setenv("NOTIFY_SOCKET", path)
	if err := Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	got := receiveOneDatagram(t, conn)
	if got != "READY=1\n" {
		t.Fatalf("received %q, want READY=1\\n", got)
	}
}

func TestReadySendsSTATUSToo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()

	t.Setenv("NOTIFY_SOCKET", path)
	if err := Ready("all systems go"); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	got := receiveOneDatagram(t, conn)
	if got != "READY=1\nSTATUS=all systems go\n" {
		t.Fatalf("received %q", got)
	}
}

func TestReadyOnAbstractSocket(t *testing.T) {
	name := fmt.Sprintf("initd-notify-test-%d", os.Getpid())
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: "\x00" + name, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()

	t.Setenv("NOTIFY_SOCKET", "@"+name)
	if err := Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	got := receiveOneDatagram(t, conn)
	if got != "READY=1\n" {
		t.Fatalf("received %q, want READY=1\\n", got)
	}
}

func TestReadyWithoutNotifySocketIsSilentNoOp(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := Ready(); err != nil {
		t.Fatalf("Ready with NOTIFY_SOCKET unset must be a no-op, got %v", err)
	}
}

func TestReadyReportsUnreachableSocket(t *testing.T) {
	// Nothing is listening on this path; the error must surface, not hang.
	t.Setenv("NOTIFY_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
	if err := Ready(); err == nil {
		t.Fatal("Ready against a dead socket must error, got nil")
	}
}
