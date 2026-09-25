package ipc

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"initd/internal/supervisor"
)

// testManager is newTestManager reduced to the manager these cases need; the
// unit search dir is irrelevant to a raw listener.
func testManager(t *testing.T) *supervisor.Manager {
	t.Helper()
	m, _ := newTestManager(t)
	return m
}

// shrinkOwnershipCheck makes the listener's watchdog fast enough to test
// without every case sleeping for the production interval.
func shrinkOwnershipCheck(t *testing.T) {
	t.Helper()
	prev := socketOwnershipCheckInterval
	socketOwnershipCheckInterval = 20 * time.Millisecond
	t.Cleanup(func() { socketOwnershipCheckInterval = prev })
}

// waitForSocketPath blocks until the path is a connectable socket, which is the
// only proof the listener is actually serving rather than merely bound.
func waitForSocketPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("unix", path, 200*time.Millisecond); err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("socket %s never became connectable", path)
}

// The failure this guards: something outside the daemon unlinks the control
// socket while the daemon is serving it. The listener keeps accepting on the
// unlinked inode, so from the inside nothing looks wrong, while every new
// client gets ENOENT from the path - a live daemon that answers no systemctl
// at all. Serve must notice and hand the address back for a rebind.
func TestServeReportsLostSocketPath(t *testing.T) {
	shrinkOwnershipCheck(t)
	path := filepath.Join(t.TempDir(), "initd.sock")

	errCh := make(chan error, 1)
	go func() { errCh <- Serve(path, testManager(t)) }()
	waitForSocketPath(t, path)

	// Exactly what a stray rm, a tmpfs cleanup or an over-eager installer does.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove socket: %v", err)
	}
	if _, err := os.Lstat(path); err == nil {
		t.Fatal("socket path still present after remove")
	}

	select {
	case err := <-errCh:
		if !IsSocketPathLost(err) {
			t.Fatalf("Serve returned %v, want errSocketPathLost", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve kept running after its path was unlinked")
	}
}

// And the recovery: the caller's retry loop rebinds the same address, so the
// control plane comes back on its own instead of staying dark until restart.
func TestServeRebindsAfterPathLoss(t *testing.T) {
	shrinkOwnershipCheck(t)
	path := filepath.Join(t.TempDir(), "initd.sock")
	stop := make(chan struct{})
	serveErr := make(chan error, 1)
	go func() {
		// The shape of cmd/initd's serveManager loop, reduced to the one rule
		// that matters: a lost path is rebound at once, without backoff.
		for {
			err := Serve(path, testManager(t))
			if err == nil {
				return
			}
			if !IsSocketPathLost(err) {
				serveErr <- err
				return
			}
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	defer close(stop)

	waitForSocketPath(t, path)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove socket: %v", err)
	}

	// No restart, no external help: the path has to come back by itself.
	waitForSocketPath(t, path)
	select {
	case err := <-serveErr:
		t.Fatalf("serve loop gave up: %v", err)
	default:
	}
}

// A socket that some other live daemon is serving must be left completely
// alone. Deleting the name would hide a working supervisor without stopping it,
// which is the same outage in the other direction.
func TestPrepareControlSocketPathLeavesLiveListener(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "initd.sock")
	other, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer other.Close()

	err = prepareControlSocketPath(path, testManager(t))
	if err == nil {
		t.Fatal("prepareControlSocketPath accepted a path held by a live listener")
	}
	if _, statErr := os.Lstat(path); statErr != nil {
		t.Errorf("live listener's socket was removed: %v", statErr)
	}
}

// A symlink dropped at the socket path is never followed and never removed: the
// path is not ours to reinterpret, and unlinking it would either destroy
// something a user put there or remove a link pointing anywhere on disk.
func TestPrepareControlSocketPathRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "precious")
	if err := os.WriteFile(victim, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	path := filepath.Join(dir, "initd.sock")
	if err := os.Symlink(victim, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := prepareControlSocketPath(path, testManager(t)); err == nil {
		t.Fatal("prepareControlSocketPath accepted a symlink")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("symlink was removed: %v", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("symlink target was harmed: %v", err)
	}
}

// A stale socket (the file is there, nobody is listening) is the one thing
// that must be cleared, or the bind fails with EADDRINUSE forever.
func TestPrepareControlSocketPathReplacesStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "initd.sock")
	dead, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Close the listener but deliberately leave the socket file behind, which
	// is what a killed daemon leaves on disk.
	dead.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := dead.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("stale socket file should still be there: %v", err)
	}

	if err := prepareControlSocketPath(path, testManager(t)); err != nil {
		t.Fatalf("prepareControlSocketPath on a stale socket: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("stale socket still present (err=%v), want it cleared", err)
	}
}

// A regular file at the socket path is debris from an interrupted start, and
// the bind needs the name - but it is still a deletion, so it is worth pinning
// that it is the regular-file case doing it and not a blanket unlink.
func TestPrepareControlSocketPathReplacesRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "initd.sock")
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := prepareControlSocketPath(path, testManager(t)); err != nil {
		t.Fatalf("prepareControlSocketPath on a regular file: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("regular file still present (err=%v), want it cleared", err)
	}
}

// A directory at the socket path is neither debris nor ours: report it instead
// of trying to remove a directory that may hold something.
func TestPrepareControlSocketPathRefusesDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "initd.sock")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := prepareControlSocketPath(path, testManager(t)); err == nil {
		t.Fatal("prepareControlSocketPath accepted a directory")
	}
	if fi, err := os.Lstat(path); err != nil || !fi.IsDir() {
		t.Errorf("directory was removed (err=%v), want it reported instead", err)
	}
}

// An absent path is the normal case and must not be reported as a problem.
func TestPrepareControlSocketPathAcceptsAbsentPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "initd.sock")
	if err := prepareControlSocketPath(path, testManager(t)); err != nil {
		t.Errorf("prepareControlSocketPath on an absent path: %v", err)
	}
}

// Two daemons must never both consider the address theirs. The second one has
// to be told the address is taken rather than quietly replacing the first.
func TestServeRefusesSecondListenerOnLiveSocket(t *testing.T) {
	shrinkOwnershipCheck(t)
	path := filepath.Join(t.TempDir(), "initd.sock")
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			if err := Serve(path, testManager(t)); err == nil || IsSocketPathLost(err) {
				select {
				case <-stop:
					return
				default:
				}
				continue
			}
			return
		}
	}()
	waitForSocketPath(t, path)

	m := testManager(t)
	err := Serve(path, m)
	if err == nil {
		t.Fatal("second Serve on a live socket succeeded")
	}
	if IsSocketPathLost(err) {
		t.Errorf("second Serve reported a lost path: %v", err)
	}
	// The incumbent must be untouched and still reachable.
	waitForSocketPath(t, path)
}
