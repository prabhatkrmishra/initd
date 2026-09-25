package userpaths

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func RealHome() string {
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		if u, err := user.Lookup(sudoUser); err == nil && u.HomeDir != "" {
			return u.HomeDir
		}
	}
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	return "/tmp"
}

// RealUID returns the UID of the user the daemon is acting for. When started
// via sudo it is the sudo user's UID, so the user socket path matches what
// that user's systemctl --user will look for; otherwise it is the current
// process UID.
func RealUID() int {
	if sudoUID := os.Getenv("SUDO_UID"); sudoUID != "" {
		if uid, err := strconv.Atoi(sudoUID); err == nil {
			return uid
		}
	}
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		if u, err := user.Lookup(sudoUser); err == nil {
			if uid, err := strconv.Atoi(u.Uid); err == nil {
				return uid
			}
		}
	}
	return os.Getuid()
}

func UserConfigHome() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return xdg
	}
	return filepath.Join(RealHome(), ".config")
}

func UserUnitsPaths() []string {
	return []string{
		filepath.Join(UserConfigHome(), "systemd/user"),
		"/etc/systemd/user",
		"/usr/lib/systemd/user",
	}
}

func UserEnabledRoot() string {
	return filepath.Join(UserConfigHome(), "systemd/user")
}

func SystemSocketPath() string {
	if os.Getuid() != 0 {
		if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
			return filepath.Join(xdg, "initd-system.sock")
		}
		uid := os.Getuid()
		runUser := fmt.Sprintf("/run/user/%d", uid)
		if st, err := os.Stat(runUser); err == nil && st.IsDir() {
			return filepath.Join(runUser, "initd-system.sock")
		}
		return fmt.Sprintf("/tmp/initd-%d-system.sock", uid)
	}
	return "/run/initd.sock"
}

func UserSocketPath() string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "initd.sock")
	}
	uid := RealUID()
	runUser := fmt.Sprintf("/run/user/%d", uid)
	if st, err := os.Stat(runUser); err == nil && st.IsDir() {
		return filepath.Join(runUser, "initd.sock")
	}
	return fmt.Sprintf("/tmp/initd-%d.sock", uid)
}

func UserRuntimeDir() string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return xdg
	}
	uid := RealUID()
	runUser := fmt.Sprintf("/run/user/%d", uid)
	if st, err := os.Stat(runUser); err == nil && st.IsDir() {
		return runUser
	}
	return fmt.Sprintf("/tmp/initd-%d", uid)
}

func UserLockPath() string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "initd.lock")
	}
	uid := RealUID()
	runUser := fmt.Sprintf("/run/user/%d", uid)
	if st, err := os.Stat(runUser); err == nil && st.IsDir() {
		return filepath.Join(runUser, "initd.lock")
	}
	return fmt.Sprintf("/tmp/initd-%d.lock", uid)
}

// SystemJournalDir is the durable log dir for the system manager. Root gets
// /var/log/initd/journal; non-root falls back to the per-user state dir so
// the daemon never fails to start for want of a writable /var/log.
func SystemJournalDir() string {
	if os.Getuid() == 0 {
		return "/var/log/initd/journal"
	}
	return filepath.Join(UserStateDir(), "journal")
}

// UserJournalDir is the durable log dir for the user manager.
func UserJournalDir() string {
	return filepath.Join(UserStateDir(), "journal")
}

// UserCacheDir follows XDG ($XDG_CACHE_HOME or ~/.cache), falling back to
// the runtime dir when HOME is unavailable (minimal chroots).
func UserCacheDir() string {
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		return filepath.Join(xdg, "initd")
	}
	home := RealHome()
	if home != "" && home != "/tmp" {
		return filepath.Join(home, ".cache", "initd")
	}
	return filepath.Join(UserRuntimeDir(), "cache")
}

// UserStateBase and UserCacheBase are the roots a *unit's* StateDirectory=,
// CacheDirectory= and %S/%C/%L specifiers hang off. They deliberately carry
// no initd segment: systemd puts those directly at $XDG_STATE_HOME/<name>,
// and a unit that moves from systemd to initd (or runs beside it) must find
// the state it already wrote.
func UserStateBase() string {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return xdg
	}
	home := RealHome()
	if home != "" && home != "/tmp" {
		return filepath.Join(home, ".local", "state")
	}
	return UserRuntimeDir()
}

func UserCacheBase() string {
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		return xdg
	}
	home := RealHome()
	if home != "" && home != "/tmp" {
		return filepath.Join(home, ".cache")
	}
	return UserRuntimeDir()
}

// UserStateDir follows XDG ($XDG_STATE_HOME or ~/.local/state), falling back
// to the runtime dir when HOME is unavailable (minimal chroots).
func UserStateDir() string {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "initd")
	}
	home := RealHome()
	if home != "" && home != "/tmp" {
		return filepath.Join(home, ".local", "state", "initd")
	}
	return filepath.Join(UserRuntimeDir(), "state")
}

func AcquireUserLock() (*os.File, error) {
	path := UserLockPath()
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	_ = f.Truncate(0)
	_, _ = f.WriteString(fmt.Sprintf("%d\n", os.Getpid()))
	return f, nil
}

func SystemLockPath() string {
	sock := SystemSocketPath()
	if strings.HasPrefix(sock, "@") {
		return ""
	}
	return sock + ".lock"
}

func AcquireSystemLock() (*os.File, error) {
	path := SystemLockPath()
	if path == "" {
		return nil, fmt.Errorf("abstract socket has no lock file")
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	_ = f.Truncate(0)
	_, _ = f.WriteString(fmt.Sprintf("%d\n", os.Getpid()))
	return f, nil
}

func IsSystemDaemonRunning() bool {
	path := SystemSocketPath()
	if strings.HasPrefix(path, "@") {
		return false
	}
	if _, err := os.Stat(path); err != nil {
		return false
	}
	conn, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return false
	}
	defer syscall.Close(conn)
	sa := &syscall.SockaddrUnix{Name: path}
	if err := syscall.Connect(conn, sa); err != nil {
		return false
	}
	return true
}

func IsUserDaemonRunning() bool {
	path := UserSocketPath()
	if strings.HasPrefix(path, "@") {
		return false
	}
	if _, err := os.Stat(path); err != nil {
		return false
	}
	conn, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return false
	}
	defer syscall.Close(conn)
	sa := &syscall.SockaddrUnix{Name: path}
	if err := syscall.Connect(conn, sa); err != nil {
		return false
	}
	return true
}
