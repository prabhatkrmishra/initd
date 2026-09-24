package ipc

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"initd/internal/userpaths"
)

const DefaultTimeout = 60 * time.Second

type Client struct {
	SocketPath string
	Timeout    time.Duration // 0 means DefaultTimeout
}

func (c *Client) effectiveTimeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	if c.Timeout < 0 {
		return 0 // no deadline
	}
	return DefaultTimeout
}

func (c *Client) Do(req Request) (Response, error) {
	socketPath := c.SocketPath
	if strings.HasPrefix(socketPath, "@") {
		socketPath = "\x00" + strings.TrimPrefix(socketPath, "@")
	}
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		// Mirror Serve's abstract fallback for >90ch filesystem paths:
		// the daemon may be listening on @initd-{user,system}-<uid>.sock
		// while the client still dials the long filesystem path. Only
		// the guessed scope is tried: falling through to the other
		// scope would silently answer a user query from the system
		// daemon (or vice versa).
		if fallback, ok := abstractDialFallback(c.SocketPath); ok {
			if fconn, ferr := net.Dial("unix", fallback); ferr == nil {
				conn = fconn
				err = nil
			}
		}
		if err != nil {
			return Response{}, err
		}
	}
	defer conn.Close()
	if d := c.effectiveTimeout(); d > 0 {
		_ = conn.SetDeadline(time.Now().Add(d))
	}

	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(conn)

	if err := encoder.Encode(req); err != nil {
		return Response{}, err
	}

	var resp Response
	if err := decoder.Decode(&resp); err != nil {
		return Response{}, err
	}
	return resp, nil
}

// abstractDialFallback maps a long filesystem socket path to the abstract
// name Serve falls back to, or false when no fallback applies.
func abstractDialFallback(socketPath string) (string, bool) {
	if len(socketPath) <= 90 || strings.HasPrefix(socketPath, "@") {
		return "", false
	}
	// Heuristic mirrors userpaths: user sockets end in initd.sock,
	// system sockets in initd-system.sock or /run/initd.sock.
	base := socketPath
	if strings.HasSuffix(base, "initd-system.sock") || base == "/run/initd.sock" {
		return fmt.Sprintf("\x00initd-system-%d.sock", os.Getuid()), true
	}
	return fmt.Sprintf("\x00initd-user-%d.sock", userpaths.RealUID()), true
}
