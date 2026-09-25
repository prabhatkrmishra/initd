package ipc

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"initd/internal/userpaths"
)

// An instance that is not the socket's owner must leave the path alone.
// Unlinking it hid a live supervisor without stopping it: already-connected
// clients kept working while every new `systemctl` saw "no such file or
// directory", which is how a running gateway read as a dead one.
func TestServeRefusesToUnlinkLiveSocket(t *testing.T) {
	m, _ := newTestManager(t)
	path := filepath.Join(t.TempDir(), "initd.sock")
	live, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer live.Close()

	done := make(chan error, 1)
	go func() { done <- Serve(path, m) }()

	select {
	case serr := <-done:
		if serr == nil || !strings.Contains(serr.Error(), "live listener") {
			t.Fatalf("Serve = %v, want a refusal naming the live listener", serr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve blocked instead of reporting the socket as owned")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the live socket path was removed: %v", err)
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatalf("the live listener became unreachable: %v", err)
	}
	_ = conn.Close()
}

// The opposite case: a socket file whose owner is gone must be replaced, or
// the bind fails with EADDRINUSE forever and the uid can never get a daemon.
func TestServeReplacesStaleSocket(t *testing.T) {
	m, _ := newTestManager(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "initd.sock")

	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		_ = syscall.Close(fd)
		t.Fatalf("bind: %v", err)
	}
	// Closed without listen() and without unlinking: exactly what a daemon
	// killed with SIGKILL leaves behind.
	_ = syscall.Close(fd)

	go func() { _ = Serve(path, m) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, derr := net.DialTimeout("unix", path, 200*time.Millisecond)
		if derr == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Serve never bound over the stale socket file")
}

// A client that cannot reach a daemon says so the way systemctl does, so a
// verb that merely prints the error still tells the operator the transport
// failed rather than looking like an answer about the units.
func TestClientBusConnectErrorWording(t *testing.T) {
	path := filepath.Join(t.TempDir(), "initd.sock")
	c := &Client{SocketPath: path}
	_, err := c.Do(Request{Action: "status"})
	if err == nil {
		t.Fatal("Do() against a missing socket succeeded")
	}
	want := "Failed to connect to system scope bus via local transport: No such file or directory"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Error("the errno cause must stay inspectable through Unwrap")
	}
}

func TestSocketScopeLabels(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"/run/user/1000/initd.sock", "user"},
		{userpaths.UserSocketPath(), "user"},
		{"/run/initd.sock", "system"},
		{"/run/user/1000/initd-system.sock", "user"},
		{"/tmp/whatever.sock", "system"},
	} {
		if got := socketScope(tc.path); got != tc.want {
			t.Errorf("socketScope(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}
