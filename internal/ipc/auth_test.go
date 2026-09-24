package ipc

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// Same-UID peers (and root) pass; anyone else is rejected before dispatch.
func TestAllowedPeer(t *testing.T) {
	self := uint32(os.Getuid())
	if !allowedPeer(nil, self) {
		t.Fatalf("own UID %d should be allowed", self)
	}
	if !allowedPeer(nil, 0) {
		t.Fatalf("root should always be allowed")
	}
	// Pick a UID that is neither us nor root.
	other := uint32(424242)
	if int(other) == os.Getuid() || other == 0 {
		other = 434343
	}
	if allowedPeer(nil, other) {
		t.Fatalf("foreign UID %d must be rejected", other)
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
