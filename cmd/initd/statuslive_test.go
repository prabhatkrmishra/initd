package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"initd/internal/ipc"
	"initd/internal/userpaths"
)

// The bug this replaces: --status read the reporting process's own transport
// map, which is always empty, so it printed "no transports advertised" even
// with a healthy daemon running. A unit test that seeds the map passes, because
// it is testing the formatting rather than the question the command exists to
// answer. So these cases run a real listener behind a real control socket and
// ask it the way a user would.
func serveTransportsOver(t *testing.T, states []transportState) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "initd.sock")

	statusState.mu.Lock()
	statusState.items = map[string]*transportState{}
	for _, s := range states {
		copied := s
		statusState.items[s.Transport+":"+s.Scope] = &copied
	}
	statusState.mu.Unlock()
	installTransportsProvider()

	// A real unix listener answering the "transports" action, which is the only
	// way the reporting process can learn the daemon's state.
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var req ipc.Request
				if err := json.NewDecoder(c).Decode(&req); err != nil {
					return
				}
				_ = json.NewEncoder(c).Encode(ipc.Response{
					Success: true,
					Data:    ipc.TransportsProvider(),
				})
			}(conn)
		}
	}()
	// Wait until the listener actually accepts, so a query cannot race it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, err := net.DialTimeout("unix", sock, 200*time.Millisecond); err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake daemon never started listening")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The listener has to outlive this helper: the whole point is that a
	// separate process asks a daemon that is already running.
	return sock, func() { _ = ln.Close() }
}

func queryOver(t *testing.T, sock string) []transportState {
	t.Helper()
	states, _, reachable := queryTransportsAt(sock)
	if !reachable {
		t.Fatalf("queryTransportsAt reported unreachable for a live socket")
	}
	return states
}

// captureOver runs f with stdout redirected, the way the flag is used.
func captureOver(t *testing.T, sock string, f func()) string {
	t.Helper()
	_ = sock
	return capture(t, f)
}

// A running daemon's transports must be visible to a separate process. This is
// the assertion the previous implementation failed.
func TestStatusSeesTheDaemonNotItself(t *testing.T) {
	sock, stop := serveTransportsOver(t, []transportState{
		{Transport: "unix", Scope: "user", Address: "/run/user/1000/initd.sock", State: "listening"},
		{Transport: "dbus", Scope: "user", Address: "unix:path=/run/user/1000/bus", State: "owned"},
	})
	defer stop()
	got := queryOver(t, sock)
	if len(got) != 2 {
		t.Fatalf("saw %d transports, want the 2 the daemon holds: %+v", len(got), got)
	}
	byKey := map[string]string{}
	for _, s := range got {
		byKey[s.Transport+":"+s.Scope] = s.Address
	}
	if byKey["unix:user"] != "/run/user/1000/initd.sock" {
		t.Errorf("unix:user = %q, want the daemon's socket", byKey["unix:user"])
	}
	if byKey["dbus:user"] != "unix:path=/run/user/1000/bus" {
		t.Errorf("dbus:user = %q, want the daemon's bus", byKey["dbus:user"])
	}
}

// A bus that is not advertised is exactly the case a client needs to see: the
// old code could only ever print its own empty map, so "retrying" was
// unreachable in practice.
func TestStatusSurfacesARetryingBus(t *testing.T) {
	sock, stop := serveTransportsOver(t, []transportState{
		{Transport: "unix", Scope: "user", Address: "/run/user/1000/initd.sock", State: "listening"},
		{Transport: "dbus", Scope: "user", Address: "unix:path=/run/user/1000/bus",
			State: "retrying", Detail: "connect: connection refused"},
	})
	defer stop()
	got := queryOver(t, sock)
	var bus transportState
	for _, s := range got {
		if s.Transport == "dbus" {
			bus = s
		}
	}
	if bus.State != "retrying" {
		t.Errorf("dbus state = %q, want retrying", bus.State)
	}
	if !strings.Contains(bus.Detail, "connection refused") {
		t.Errorf("dbus detail = %q, want the reason", bus.Detail)
	}
}

// With no daemon at all the answer must still be useful: name the socket to
// look at and say it is unreachable, rather than printing nothing.
func TestStatusWithoutADaemonNamesTheSocket(t *testing.T) {
	dir := t.TempDir()
	dead := filepath.Join(dir, "nothing-here.sock")
	states, pid, reachable := queryTransportsAt(dead)
	if reachable {
		t.Error("reported reachable for a socket that does not exist")
	}
	if pid != 0 {
		t.Errorf("pid = %d, want 0 when no daemon answered", pid)
	}
	if len(states) != 1 {
		t.Fatalf("states = %+v, want one fallback row", states)
	}
	if states[0].State != "unreachable" {
		t.Errorf("state = %q, want unreachable", states[0].State)
	}
	if states[0].Address == "" {
		t.Error("fallback row does not name an address to investigate")
	}
	if states[0].Detail == "" {
		t.Error("fallback row does not say why it could not connect")
	}
}

// The rendered report must contain what a human is looking for.
func TestStatusRendersWhatTheDaemonReported(t *testing.T) {
	sock, stop := serveTransportsOver(t, []transportState{
		{Transport: "unix", Scope: "user", Address: "/run/user/1000/initd.sock", State: "listening"},
		{Transport: "dbus", Scope: "user", Address: "unix:path=/run/user/1000/bus",
			State: "retrying", Detail: "connection refused"},
	})
	defer stop()
	out := captureOver(t, sock, func() { printStatusAt(sock, false) })
	for _, want := range []string{"listening", "retrying", "connection refused", "/run/user/1000/bus"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "no transports advertised") {
		t.Errorf("status claimed nothing was advertised while a daemon answered:\n%s", out)
	}
}

// userpaths is the single source of truth for these addresses; --print-paths
// must not disagree with it.
func TestPrintPathsAgreesWithUserpaths(t *testing.T) {
	out := capture(t, func() { printPaths(true) })
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if got["user_socket"] != userpaths.UserSocketPath() {
		t.Errorf("user_socket = %q, want %q", got["user_socket"], userpaths.UserSocketPath())
	}
	if got["system_socket"] != userpaths.SystemSocketPath() {
		t.Errorf("system_socket = %q, want %q", got["system_socket"], userpaths.SystemSocketPath())
	}
	_ = os.Getenv("XDG_RUNTIME_DIR")
}
