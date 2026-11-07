package ipc

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The control policy admits root and the daemon's own uid; anyone else is
// rejected even for a read verb, matching the 0600 socket.
func TestCheckActionControl(t *testing.T) {
	control := socketAccess{gid: -1}
	self := uint32(os.Getuid())
	if err := control.checkAction(self, true, "status"); err != nil {
		t.Fatalf("own UID %d should be allowed: %v", self, err)
	}
	if err := control.checkAction(0, true, "stop"); err != nil {
		t.Fatalf("root should be allowed: %v", err)
	}
	// Pick a UID that is neither us nor root.
	other := uint32(424242)
	if int(other) == os.Getuid() || other == 0 {
		other = 434343
	}
	if err := control.checkAction(other, true, "status"); err == nil {
		t.Fatalf("foreign UID %d must be rejected", other)
	}
	// Without kernel credentials the filesystem bits are the gate (tests).
	if err := control.checkAction(0, false, "stop"); err != nil {
		t.Fatalf("missing credentials should defer to permissions: %v", err)
	}
}

// The read-only listener admits read verbs for any peer the kernel let in,
// and refuses every mutating verb, root included.
func TestCheckActionReadOnly(t *testing.T) {
	read := socketAccess{readOnly: true, gid: -1}
	for _, action := range []string{
		"status", "show", "is-active", "is-enabled", "is-failed", "cat",
		"is-system-running", "transports", "need-daemon-reload", "journal",
		"journal-boots", "journal-usage", "logs", "list-units",
		"list-unit-files",
	} {
		if err := read.checkAction(0, true, action); err != nil {
			t.Errorf("read action %q denied: %v", action, err)
		}
	}
	for _, action := range []string{
		"start", "stop", "restart", "reload", "enable", "disable",
		"reset-failed", "mask", "unmask", "kill", "daemon-reload",
		"journal-vacuum", "journal-sync", "journal-rotate", "reboot",
		"poweroff", "halt", "frobnicate", "",
	} {
		if err := read.checkAction(0, true, action); err == nil {
			t.Errorf("mutating action %q must be denied", action)
		}
	}
}

// Kernel credentials on a real Unix socket must resolve to our own UID.
func TestPeerUIDSelf(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cred.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	type res struct {
		uid uint32
		ok  bool
	}
	ch := make(chan res, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		uid, ok := peerUID(c)
		ch <- res{uid, ok}
	}()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := <-ch
	if !r.ok {
		t.Fatalf("peerUID failed on real unix socket")
	}
	if int(r.uid) != os.Getuid() {
		t.Fatalf("peer UID = %d, want %d", r.uid, os.Getuid())
	}
}

// A read-only listener must stay group-connectable while refusing the same
// peer every mutating verb.
func TestServeReadOnlyAllowsReadsDeniesMutations(t *testing.T) {
	shrinkOwnershipCheck(t)
	path := filepath.Join(t.TempDir(), "initd-read.sock")
	m := testManager(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ServeReadOnly(path, m, os.Getegid())
	}()
	// Removing the path is what makes Serve return: it treats a vanished
	// path as its cue to hand the address back for a rebind.
	defer func() {
		_ = os.Remove(path)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("read-only listener did not stop")
		}
	}()
	waitForSocketPath(t, path)

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat read socket: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0660 {
		t.Fatalf("read socket mode = %04o, want 0660", perm)
	}

	client := &Client{SocketPath: path}
	resp, err := client.Do(Request{Action: "list-units"})
	if err != nil || !resp.Success {
		t.Fatalf("read over read-only socket = %+v, %v", resp, err)
	}
	resp, err = client.Do(Request{Action: "start", Unit: "app.service"})
	if err != nil {
		t.Fatalf("mutation request: %v", err)
	}
	if resp.Success || !strings.Contains(resp.Message, "read-only") {
		t.Fatalf("mutation on read-only socket = %+v, want denial", resp)
	}
}
