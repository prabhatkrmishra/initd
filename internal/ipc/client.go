package ipc

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
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
			return Response{}, &busConnectError{scope: socketScope(c.SocketPath), cause: err}
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

// Scope labels the transport this client dials as "user" or "system", the
// same rule used to word a connection failure so a hint printed next to that
// failure names the same scope.
func (c *Client) Scope() string { return socketScope(c.SocketPath) }

// busConnectError reports an unreachable daemon the way systemctl does: the
// transport failed, so a caller must not present the units it asked about as
// idle. Rendering it here gives every verb the upstream wording without
// each one re-detecting a dead socket.
type busConnectError struct {
	scope string
	cause error
}

func (e *busConnectError) Error() string {
	return fmt.Sprintf("Failed to connect to %s scope bus via local transport: %s",
		e.scope, transportReason(e.cause))
}

func (e *busConnectError) Unwrap() error { return e.cause }

// transportReason reduces a Go dial error to the errno text systemctl prints
// ("No such file or directory", "Connection refused").
func transportReason(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return capitalise(errno.Error())
	}
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		msg = msg[i+2:]
	}
	return capitalise(msg)
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// socketScope labels a socket path the way the daemon names it: the user
// socket lives under $XDG_RUNTIME_DIR, anything else is system scope. A
// custom --socket that is neither gets reported as system, which is the only
// thing left to say about an address the manager did not choose.
func socketScope(socketPath string) string {
	if socketPath == userpaths.UserSocketPath() || strings.Contains(socketPath, "/run/user/") {
		return "user"
	}
	return "system"
}
