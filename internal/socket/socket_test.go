package socket

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"initd/internal/parser"
)

func TestStartStopStream(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.sock")
	unit := &parser.Unit{
		Name: "test.socket",
		Type: "socket",
		Socket: parser.SocketSection{
			ListenStream: []string{path},
		},
	}

	rt, err := Start(unit)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !rt.Active {
		t.Error("runtime should be active")
	}
	if rt.Network != "unix" {
		t.Errorf("Network = %q, want unix", rt.Network)
	}
	if rt.Listener == nil {
		t.Fatal("Listener should be set for stream socket")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("socket file should exist: %v", err)
	}

	// A client should be able to connect.
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()

	if err := rt.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if rt.Active {
		t.Error("runtime should be inactive after Stop")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("socket file should be removed after Stop, got %v", err)
	}
}

func TestStartStopDatagram(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.sock")
	unit := &parser.Unit{
		Name: "test.socket",
		Type: "socket",
		Socket: parser.SocketSection{
			ListenDatagram: []string{path},
		},
	}

	rt, err := Start(unit)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if rt.Network != "unixgram" {
		t.Errorf("Network = %q, want unixgram", rt.Network)
	}
	if rt.Packet == nil {
		t.Fatal("Packet should be set for datagram socket")
	}
	if err := rt.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStartNoListen(t *testing.T) {
	unit := &parser.Unit{Name: "test.socket", Type: "socket"}
	if _, err := Start(unit); err == nil {
		t.Error("Start with no ListenStream/ListenDatagram should error")
	}
}

func TestApplyMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.sock")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := applyMode(path, "0600"); err != nil {
		t.Fatalf("applyMode: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	// Invalid mode errors.
	if err := applyMode(path, "zzz"); err == nil {
		t.Error("applyMode with invalid mode should error")
	}
}

func TestPrepareSocketPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "test.sock")
	if err := prepareSocketPath(path); err != nil {
		t.Fatalf("prepareSocketPath: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Errorf("parent dir should exist: %v", err)
	}
}
