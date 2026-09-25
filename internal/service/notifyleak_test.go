package service

import (
	"os"
	"strings"
	"testing"
	"time"

	"initd/internal/notify"
)

// notifySocketCount counts the notify sockets the kernel still holds for this
// user. /proc/net/unix is the only place an abstract name shows up, so it is
// what makes the leak observable at all: a closed socket has no name left to
// grep for anywhere else.
func notifySocketCount(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile("/proc/net/unix")
	if err != nil {
		t.Skipf("cannot read /proc/net/unix: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "initd-notify-") {
			n++
		}
	}
	return n
}

// waitNotifyCount waits for the kernel to drop back to want, so a close that is
// still in flight when we look is not reported as a leak.
func waitNotifyCount(t *testing.T, want int) int {
	t.Helper()
	got := notifySocketCount(t)
	deadline := time.Now().Add(2 * time.Second)
	for got > want && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		got = notifySocketCount(t)
	}
	return got
}

// releaseNotifyServer closes the socket and drops the unit's handle, so a
// caller can tell the two apart: a stopped server that is still reachable
// through the unit would be released twice.
func TestReleaseNotifyServerClearsUnitHandle(t *testing.T) {
	u := newTestUnit(t, "zzrelnotify.service", "[Service]\nType=notify\nExecStart=/bin/sleep 30\n")
	srv, err := notify.Start()
	if err != nil {
		t.Fatalf("notify.Start: %v", err)
	}
	u.mu.Lock()
	u.notifyServer = srv
	u.mu.Unlock()

	u.releaseNotifyServer(srv)

	u.mu.Lock()
	held := u.notifyServer
	u.mu.Unlock()
	if held != nil {
		t.Error("unit still holds the notify server after release")
	}
	// Idempotent: a second release must not panic or touch a successor.
	u.releaseNotifyServer(srv)
}

// A start that never got as far as running anything must not leave a notify
// socket behind. The refused exec returns before the unit could own the
// socket, which is exactly the case the deferred cleanup exists for.
func TestNotifyStartWithRefusedExecLeavesNoSocket(t *testing.T) {
	before := waitNotifyCount(t, 0)

	u := newTestUnit(t, "zznotifyfail.service",
		"[Service]\nType=notify\nExecStart=/nonexistent-initd-test-binary\n")
	if _, err := u.StartAndWait(true); err == nil {
		t.Fatal("StartAndWait() = nil for a binary that cannot be exec'd, want error")
	}
	waitForState(t, u, StateFailed)

	if got := waitNotifyCount(t, before); got != before {
		u.mu.Lock()
		held := u.notifyServer
		u.mu.Unlock()
		t.Errorf("notify sockets after a refused exec = %d, want %d (unit handle: %v)",
			got, before, held != nil)
	}
}

// A start that is superseded between the socket being created and the token
// check has nobody left to close it: the newer start (or the stop) cleared the
// unit's handle, so the socket this start made is only reachable from here.
// The window is a few microseconds wide, so drive it the way a restart does
// and require that repeated attempts never accumulate a socket.
func TestSupersededNotifyStartDoesNotAccumulateSockets(t *testing.T) {
	before := waitNotifyCount(t, 0)

	for i := 0; i < 12; i++ {
		u := newTestUnit(t, "zzsuperseded.service",
			"[Service]\nType=notify\nExecStart=/bin/sleep 30\n")
		// Start, then immediately supersede it the way a restart or a stop
		// does. Either this start hands the socket to the unit (and the
		// cleanup below releases it) or it is superseded before the hand-off
		// (and the deferred release does). Neither may leak.
		u.Start()
		u.mu.Lock()
		u.startToken++
		u.stopRequested = true
		u.mu.Unlock()

		_ = u.Stop(2 * time.Second)
		u.releaseNotifyServer(func() *notify.Server {
			u.mu.Lock()
			defer u.mu.Unlock()
			return u.notifyServer
		}())
	}

	if got := waitNotifyCount(t, before); got != before {
		t.Errorf("notify sockets after 12 superseded starts = %d, want %d", got, before)
	}
}
