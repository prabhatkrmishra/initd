package ipc

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The hazard RemoveIfOurs exists to prevent: a daemon that is shutting down
// after a successor has already rebound the same address must not delete the
// winner's socket. That is the outage this whole file is about, caused from
// the other end - deliberately instead of by a stray rm.
func TestRemoveIfOursLeavesASuccessorsSocket(t *testing.T) {
	shrinkOwnershipCheck(t)
	path := filepath.Join(t.TempDir(), "initd.sock")

	// Stand in for the outgoing daemon: this process bound the path, and a
	// successor has since taken it over.
	recordBoundSocket(path, socketIdentity{dev: 1, ino: 1})
	successor, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("successor listen: %v", err)
	}
	defer successor.Close()
	successorID, ok := lstatIdentity(path)
	if !ok {
		t.Fatal("successor socket has no identity")
	}

	// The outgoing daemon's recorded identity no longer matches the path.
	RemoveIfOurs(path)

	if _, err := os.Lstat(path); err != nil {
		t.Errorf("successor's socket was removed: %v", err)
	}
	if conn, err := net.DialTimeout("unix", path, 500*time.Millisecond); err != nil {
		t.Errorf("successor is no longer reachable: %v", err)
	} else {
		_ = conn.Close()
	}
	_ = successorID
}

// The ordinary case still has to work: our own socket, still ours, is removed
// so the next daemon can bind without EADDRINUSE.
func TestRemoveIfOursRemovesOurOwnSocket(t *testing.T) {
	shrinkOwnershipCheck(t)
	path := filepath.Join(t.TempDir(), "initd.sock")

	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	listener.(*net.UnixListener).SetUnlinkOnClose(false)

	id, ok := lstatIdentity(path)
	if !ok {
		t.Fatal("no identity for our own socket")
	}
	recordBoundSocket(path, id)

	RemoveIfOurs(path)

	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("our own socket survived RemoveIfOurs (err=%v), want it removed", err)
	}
}

// A path this process never bound is none of its business - not even if it
// happens to hold a live socket.
func TestRemoveIfOursIgnoresUnboundPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "initd.sock")
	other, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer other.Close()

	RemoveIfOurs(path)

	if _, err := os.Lstat(path); err != nil {
		t.Errorf("an unbound path was removed: %v", err)
	}
}

// StopServing has to actually stop the listener, or a shutdown that removes the
// name first would leave the daemon accepting on an unlinked inode - the exact
// state this package exists to prevent.
func TestStopServingClosesOurListener(t *testing.T) {
	shrinkOwnershipCheck(t)
	path := filepath.Join(t.TempDir(), "initd.sock")

	serveErr := make(chan error, 1)
	go func() { serveErr <- Serve(path, testManager(t)) }()
	waitForSocketPath(t, path)

	StopServing(path)

	select {
	case err := <-serveErr:
		// Any return is fine; what matters is that it returned rather than
		// lingering in Accept.
		if err == nil {
			t.Log("Serve returned nil after StopServing")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StopServing did not unblock Accept")
	}
}

// StopServing on a path we never bound must be a no-op, not a panic: shutdown
// calls it for both the system and user socket regardless of which this
// process ended up serving.
func TestStopServingIgnoresUnboundPath(t *testing.T) {
	StopServing(filepath.Join(t.TempDir(), "never-bound.sock"))
}
