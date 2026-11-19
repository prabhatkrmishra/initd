package notify

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"initd/internal/userpaths"
)

// socketMode is what systemd gives its notification socket. A service often
// runs as an unprivileged User=, so the socket has to be writable by it; the
// directory is not, and the name is unique per start.
const socketMode = 0o666

var seq atomic.Uint64

type Server struct {
	Path  string
	Conn  *net.UnixConn
	Ready chan struct{}

	once sync.Once
}

// Dir is the directory holding this manager's per-start notification sockets.
// The system manager uses /run/initd, a user manager its own runtime dir.
func Dir() string {
	if os.Getuid() != 0 {
		return filepath.Join(userpaths.UserRuntimeDir(), "initd", "notify")
	}
	return "/run/initd/notify"
}

// Start creates the notification socket for one service start.
//
// The socket lives on the filesystem rather than in the abstract namespace.
// Real systemd hands services an absolute NOTIFY_SOCKET path, and not every
// notifier implements the abstract form: this host's sshd only sends READY=1
// to a path, so an abstract socket leaves Type=notify services stuck in
// "activating" until TimeoutStartSec and then restarts them forever.
func Start() (*Server, error) {
	dir := Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	name := fmt.Sprintf("notify-%d-%d.sock", os.Getpid(), seq.Add(1))
	path := filepath.Join(dir, name)
	_ = os.Remove(path)

	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, socketMode); err != nil {
		_ = conn.Close()
		_ = os.Remove(path)
		return nil, err
	}

	s := &Server{
		Path:  path,
		Conn:  conn,
		Ready: make(chan struct{}),
	}
	go s.listen()
	return s, nil
}

// SweepStale removes notification sockets left behind by a daemon that is
// gone. The owning pid is part of the file name, so a socket belonging to
// another live manager in the same directory is never touched.
func SweepStale() {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		return
	}
	self := os.Getpid()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		pid, ok := socketOwnerPID(entry.Name())
		if !ok || pid == self {
			continue
		}
		if err := syscall.Kill(pid, 0); err == nil || errors.Is(err, syscall.EPERM) {
			continue
		}
		_ = os.Remove(filepath.Join(Dir(), entry.Name()))
	}
}

func socketOwnerPID(name string) (int, bool) {
	if !strings.HasPrefix(name, "notify-") || !strings.HasSuffix(name, ".sock") {
		return 0, false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(name, "notify-"), ".sock")
	pidPart, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0, false
	}
	pid, err := strconv.Atoi(pidPart)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func (s *Server) listen() {
	buf := make([]byte, 4096)
	for {
		n, _, err := s.Conn.ReadFromUnix(buf)
		if err != nil {
			return
		}
		msg := string(buf[:n])
		if strings.Contains(msg, "READY=1") {
			s.once.Do(func() { close(s.Ready) })
			return
		}
	}
}

func (s *Server) Stop() {
	if s.Conn != nil {
		_ = s.Conn.Close()
	}
	if s.Path != "" && !strings.HasPrefix(s.Path, "@") {
		_ = os.Remove(s.Path)
	}
}
