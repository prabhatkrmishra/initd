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
