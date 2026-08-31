package notify

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestStartStop(t *testing.T) {
	s, err := Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s.Path == "" {
		t.Error("Path should be set")
	}
	if s.Ready == nil {
		t.Error("Ready channel should be set")
	}
	s.Stop()
}

func TestReadyOnREADY1(t *testing.T) {
	s, err := Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	addr := s.Path
	if strings.HasPrefix(addr, "@") {
		addr = "\x00" + strings.TrimPrefix(addr, "@")
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("READY=1")); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case <-s.Ready:
		// Ready closed as expected.
	case <-time.After(2 * time.Second):
		t.Fatal("Ready channel did not close after READY=1")
	}
}

func TestStopClosesConnection(t *testing.T) {
	s, err := Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.Stop()
	// Stop should be idempotent and not panic.
	s.Stop()
}
