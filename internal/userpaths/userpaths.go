package userpaths

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
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
