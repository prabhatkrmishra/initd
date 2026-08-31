package userpaths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUserConfigHome(t *testing.T) {
	oldHome := os.Getenv("HOME")
	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	defer func() {
		os.Setenv("HOME", oldHome)
		os.Setenv("XDG_CONFIG_HOME", oldXDG)
	}()

	// XDG_CONFIG_HOME wins.
	os.Setenv("XDG_CONFIG_HOME", "/tmp/xdgconf")
	os.Setenv("HOME", "/home/testuser")
	if got := UserConfigHome(); got != "/tmp/xdgconf" {
		t.Errorf("UserConfigHome with XDG = %q, want /tmp/xdgconf", got)
	}

	// Falls back to ~/.config.
	os.Setenv("XDG_CONFIG_HOME", "")
	if got := UserConfigHome(); got != filepath.Join("/home/testuser", ".config") {
		t.Errorf("UserConfigHome fallback = %q", got)
	}
}

func TestUserUnitsPaths(t *testing.T) {
	oldHome := os.Getenv("HOME")
	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	defer func() {
		os.Setenv("HOME", oldHome)
		os.Setenv("XDG_CONFIG_HOME", oldXDG)
	}()
	os.Setenv("XDG_CONFIG_HOME", "/tmp/xdgconf")
	os.Setenv("HOME", "/home/testuser")

	paths := UserUnitsPaths()
	if len(paths) != 3 {
		t.Fatalf("UserUnitsPaths len = %d, want 3", len(paths))
	}
	if paths[0] != filepath.Join("/tmp/xdgconf", "systemd/user") {
		t.Errorf("UserUnitsPaths[0] = %q", paths[0])
	}
	if paths[1] != "/etc/systemd/user" {
		t.Errorf("UserUnitsPaths[1] = %q", paths[1])
	}
	if paths[2] != "/usr/lib/systemd/user" {
		t.Errorf("UserUnitsPaths[2] = %q", paths[2])
	}
}

func TestUserRuntimeDir(t *testing.T) {
	oldXDG := os.Getenv("XDG_RUNTIME_DIR")
	defer os.Setenv("XDG_RUNTIME_DIR", oldXDG)

	os.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got := UserRuntimeDir(); got != "/run/user/1000" {
		t.Errorf("UserRuntimeDir with XDG = %q", got)
	}
}

func TestSystemSocketPathRoot(t *testing.T) {
	// When running as root, the system socket is /run/initd.sock.
	if os.Getuid() == 0 {
		if got := SystemSocketPath(); got != "/run/initd.sock" {
			t.Errorf("SystemSocketPath as root = %q, want /run/initd.sock", got)
		}
	}
}

func TestUserSocketPathXDG(t *testing.T) {
	oldXDG := os.Getenv("XDG_RUNTIME_DIR")
	defer os.Setenv("XDG_RUNTIME_DIR", oldXDG)
	os.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got := UserSocketPath(); got != filepath.Join("/run/user/1000", "initd.sock") {
		t.Errorf("UserSocketPath = %q", got)
	}
}
