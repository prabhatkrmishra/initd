package ipc

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

// Oversized requests must fail fast instead of ballooning buffers.
func TestOversizedRequestRejected(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleConn(a, nil)
	}()
	huge := `{"action":"status","unit":"` + strings.Repeat("x", 2<<20) + `"}`
	// Write in the background: the server stops reading at the 1MB cap
	// while test bytes remain, so a synchronous write would block forever.
	go func() { _, _ = b.Write([]byte(huge)) }()
	_ = b.SetReadDeadline(time.Now().Add(5 * time.Second))
	var resp Response
	if err := json.NewDecoder(b).Decode(&resp); err != nil {
		t.Fatalf("no error response for oversized request: %v", err)
	}
	if resp.Success {
		t.Fatalf("oversized request must not succeed")
	}
	<-done
}
