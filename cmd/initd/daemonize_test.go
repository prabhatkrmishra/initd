package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The login hook runs on the critical path of every shell the user opens. It
// must never sit behind daemon startup, and the old code could hold a
// terminal for a flat ten seconds before printing a timeout nobody could act
// on. A detached start returns as soon as the child is confirmed started.
func TestDaemonizeDoesNotBlockByDefault(t *testing.T) {
	cfg, err := parseArgs([]string{"--init", "--daemonize", "--pid-file", "/tmp/does-not-matter.pid"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if cfg.waitReady {
		t.Error("--daemonize implies --wait-ready; a login hook would block again")
	}
}

// A caller that opts in must get it, and --wait-ready alone is meaningless
// without --daemonize.
func TestWaitReadyIsOptIn(t *testing.T) {
	cfg, err := parseArgs([]string{"--init", "--daemonize", "--wait-ready"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if !cfg.waitReady {
		t.Error("--wait-ready was not recorded")
	}
	if !cfg.daemonize {
		t.Error("--daemonize was lost")
	}
}

// Readiness is the control socket accepting, not the pid file appearing. The
// pid file is written before the manager has loaded its units, so a caller
// returning on it could immediately issue a command nobody answers yet.
func TestDaemonReadyRequiresTheSocketToAccept(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "initd.pid")
	sockPath := filepath.Join(dir, "initd.sock")
	child := os.Getpid()
	if err := os.WriteFile(pidPath, []byte(itoaPid(child)+"\n"), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	// Pid file alone is not enough: nothing is listening on the socket yet.
	if daemonReady(pidPath, sockPath, child) {
		t.Error("reported ready with a pid file but no listener")
	}

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	if !daemonReady(pidPath, sockPath, child) {
		t.Error("not ready even though the pid file and a live listener are both present")
	}
}

// A pid file naming a different process is a predecessor's, not ours: readiness
// has to be about the child we started.
func TestDaemonReadyRejectsAnotherOwnersPidFile(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "initd.pid")
	sockPath := filepath.Join(dir, "initd.sock")
	other := os.Getppid()
	if err := os.WriteFile(pidPath, []byte(itoaPid(other)+"\n"), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	if daemonReady(pidPath, sockPath, os.Getpid()) {
		t.Error("reported ready off a pid file naming another process")
	}
}

// An abstract socket has no path to dial, so the pid file is the only signal
// available. It must not deadlock the waiter.
func TestDaemonReadyAcceptsAbstractSocketOnPidFileAlone(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "initd.pid")
	if err := os.WriteFile(pidPath, []byte(itoaPid(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
	if !daemonReady(pidPath, "@initd-test.sock", os.Getpid()) {
		t.Error("abstract socket: pid file alone should be enough")
	}
}

// The wait must be bounded, but the bound belongs to the installer path - a
// login never takes it, so it can be generous rather than the old ten seconds.
func TestDaemonReadyTimeoutIsGenerousAndBounded(t *testing.T) {
	if daemonReadyTimeout <= 10*time.Second {
		t.Errorf("daemonReadyTimeout = %v; the installer path can afford longer than the old 10s", daemonReadyTimeout)
	}
	if daemonReadyTimeout > 5*time.Minute {
		t.Errorf("daemonReadyTimeout = %v; an installer should not hang for minutes", daemonReadyTimeout)
	}
}

func itoaPid(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// The real assertion: spawnDetached itself must come back promptly. Config
// tests do not catch this - the old code parsed the same flags and still
// blocked, so the test has to run the launcher against a child that never
// becomes ready, which is exactly the case that used to cost ten seconds.
func TestSpawnDetachedReturnsWithoutWaitingForReadiness(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "daemon.log")
	pidPath := filepath.Join(dir, "never-written.pid")

	// A child that starts, then sits there without ever recording a pid file:
	// the shape of a daemon that is slow to come up.
	script := filepath.Join(dir, "slowd")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	// spawnDetached resolves the binary via os.Executable, so exercise the
	// launcher by pointing it at the helper through a symlink-free name.
	cfg := daemonConfig{daemonize: true, pidFile: pidPath, logFile: logPath}
	start := time.Now()
	err := spawnDetachedAs(cfg, script)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("spawnDetachedAs: %v", err)
	}
	// The old code polled for up to 10s before giving up with a timeout error.
	// A non-blocking launcher must not come anywhere near that.
	if elapsed > 3*time.Second {
		t.Errorf("spawnDetached took %v; the launcher must not wait on readiness by default", elapsed)
	}
}

// With --wait-ready the launcher is allowed to block, and it must block until
// the socket is actually serving - not until the pid file merely appears.
func TestSpawnDetachedWaitsForTheSocketNotThePidFile(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "initd.pid")
	sockPath := filepath.Join(dir, "initd.sock")
	logPath := filepath.Join(dir, "daemon.log")

	// A child that writes the pid file immediately but never listens: the pid
	// file alone is exactly the signal the old code accepted too early.
	script := filepath.Join(dir, "pidsonly")
	body := "#!/bin/sh\nprintf '%s\\n' $$ > " + pidPath + "\nsleep 30\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write helper: %v", err)
	}

	cfg := daemonConfig{daemonize: true, waitReady: true, pidFile: pidPath,
		logFile: logPath, socketPath: sockPath}
	done := make(chan error, 1)
	go func() { done <- spawnDetachedAs(cfg, script) }()
	select {
	case err := <-done:
		t.Fatalf("returned %v on a pid file with no listener; readiness must require the socket", err)
	case <-time.After(1500 * time.Millisecond):
		// Still waiting: correct, the socket never accepted.
	}
}
