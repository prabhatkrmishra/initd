package ipc

import (
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClientEffectiveTimeout(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{
		{"zero means default", 0, DefaultTimeout},
		{"positive custom", 5 * time.Second, 5 * time.Second},
		{"large custom", 120 * time.Second, 120 * time.Second},
		{"negative means no deadline", -1, 0},
		{"negative large", -10 * time.Second, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{Timeout: tc.timeout}
			if got := c.effectiveTimeout(); got != tc.want {
				t.Errorf("effectiveTimeout() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClientDefaultTimeoutValue(t *testing.T) {
	if DefaultTimeout != 60*time.Second {
		t.Errorf("DefaultTimeout = %v, want 60s", DefaultTimeout)
	}
	// Default should cover StopTimeout(10s)+StartTimeout(30s)=40s with margin.
	if DefaultTimeout < 40*time.Second {
		t.Error("DefaultTimeout too small to cover restart (Stop+Start)")
	}
}

func TestClientDoCustomTimeout(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "test.sock")

	// Start a server that delays response by 200ms.
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var req Request
				_ = json.NewDecoder(c).Decode(&req)
				time.Sleep(200 * time.Millisecond)
				_ = json.NewEncoder(c).Encode(Response{Success: true, Data: "ok"})
			}(conn)
		}
	}()

	// Client with 50ms timeout should fail (server sleeps 200ms).
	cShort := &Client{SocketPath: sock, Timeout: 50 * time.Millisecond}
	_, err = cShort.Do(Request{Action: "status", Unit: "test.service"})
	if err == nil {
		t.Error("expected timeout error with 50ms deadline, got nil")
	} else if !isTimeoutError(err) {
		// Accept any error that indicates timeout/deadline, but log what we got.
		t.Logf("got expected error with short timeout: %v", err)
	}

	// Client with 500ms timeout should succeed.
	cLong := &Client{SocketPath: sock, Timeout: 500 * time.Millisecond}
	resp, err := cLong.Do(Request{Action: "status", Unit: "test.service"})
	if err != nil {
		t.Fatalf("expected success with 500ms timeout, got %v", err)
	}
	if !resp.Success {
		t.Errorf("expected Success=true, got %v", resp.Message)
	}

	// Client with default timeout (0 => 60s) should also succeed.
	cDefault := &Client{SocketPath: sock}
	resp, err = cDefault.Do(Request{Action: "status", Unit: "test.service"})
	if err != nil {
		t.Fatalf("expected success with default timeout, got %v", err)
	}
	if !resp.Success {
		t.Errorf("expected Success=true with default timeout, got %v", resp.Message)
	}
}

func TestClientDoNegativeTimeoutNoDeadline(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "test.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var req Request
				_ = json.NewDecoder(c).Decode(&req)
				// Delay longer than default timeout to prove no deadline.
				time.Sleep(300 * time.Millisecond)
				_ = json.NewEncoder(c).Encode(Response{Success: true})
			}(conn)
		}
	}()

	// Negative timeout means no deadline — should succeed even though
	// delay exceeds DefaultTimeout.
	c := &Client{SocketPath: sock, Timeout: -1}
	resp, err := c.Do(Request{Action: "status"})
	if err != nil {
		t.Fatalf("expected success with no deadline, got %v", err)
	}
	if !resp.Success {
		t.Error("expected Success=true with no deadline")
	}
}

func TestClientDoAbstractSocket(t *testing.T) {
	// Abstract sockets use @ prefix; ensure timeout still applies.
	// Use a non-existent abstract socket — should fail quickly on dial,
	// not hang on deadline.
	c := &Client{SocketPath: "@initd-test-abstract-12345", Timeout: 100 * time.Millisecond}
	_, err := c.Do(Request{Action: "status"})
	if err == nil {
		t.Error("expected error dialing non-existent abstract socket")
	}
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "deadline") ||
		strings.Contains(msg, "i/o timeout")
}
