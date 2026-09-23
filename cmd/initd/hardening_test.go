package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestDirModeFor(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got := dirModeFor("/run/user/1000/initd.pid"); got != 0o700 {
		t.Fatalf("runtime dir mode = %o, want 700", got)
	}
	if got := dirModeFor("/run/user/7/x"); got != 0o700 {
		t.Fatalf("run-user mode = %o, want 700", got)
	}
	if got := dirModeFor("/run/initd.sock"); got != 0o755 {
		t.Fatalf("system path mode = %o, want 755", got)
	}
}

func TestMaybeRotateLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "initd.log")
	if err := os.WriteFile(path, []byte("small"), 0o644); err != nil {
		t.Fatal(err)
	}
	maybeRotateLog(path)
	if _, err := os.Stat(path + ".prev"); !os.IsNotExist(err) {
		t.Fatal("small log should not rotate")
	}
	big := make([]byte, maxDetachedLog+1)
	if err := os.WriteFile(path, big, 0o644); err != nil {
		t.Fatal(err)
	}
	maybeRotateLog(path)
	if _, err := os.Stat(path + ".prev"); err != nil {
		t.Fatalf("big log should rotate: %v", err)
	}
}

func TestChildWrotePidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "initd.pid")
	if childWrotePidFile(path, os.Getpid()) {
		t.Fatal("missing file should not match")
	}
	if err := os.WriteFile(path, []byte("99999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if childWrotePidFile(path, os.Getpid()) {
		t.Fatal("stale pid should not match")
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !childWrotePidFile(path, os.Getpid()) {
		t.Fatal("own pid should match")
	}
}
