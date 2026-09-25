package service

import (
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The shell wrap is the only thing that can put a daemon's own pid into
// $LISTEN_PID, and sd_listen_fds() answers "no fds" to a process whose pid does
// not match it. Starting anyway would hand the daemon a socket it cannot see: a
// unit that reports active and never accepts a connection. So a start that
// cannot build the wrap has to fail instead.
func TestSocketActivationWithoutAShellFailsTheStart(t *testing.T) {
	orig := socketWrapShell
	socketWrapShell = filepath.Join(t.TempDir(), "no-such-shell")
	t.Cleanup(func() { socketWrapShell = orig })

	u := socketActivatedUnit(t, "zzsock.service", sleepUnitContent())
	if _, err := u.Start(); err != nil {
		t.Logf("Start reported: %v", err)
	}
	waitForState(t, u, StateFailed)
	if pid := u.Snapshot().MainPID; pid != 0 {
		t.Errorf("a start that could not hand over its fds left MainPID=%d", pid)
	}
}

// waitForActive starts a Type=simple unit and waits for the record of it: a
// simple job is answered by the fork, so the main pid is only in the unit a
// moment after Start returns.
func waitForActive(t *testing.T, u *Unit) int {
	t.Helper()
	if _, err := u.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForState(t, u, StateActive)
	pid := u.Snapshot().MainPID
	if pid <= 0 {
		t.Fatal("no main pid for a socket-activated start")
	}
	return pid
}

// The same unit with a shell: the wrap is what makes the start work, so the
// guard above must not be what every socket-activated service hits. The child's
// own environment is read back from /proc, because what the supervisor meant to
// pass is not the same fact.
func TestSocketActivationExportsTheDaemonsOwnPID(t *testing.T) {
	u := socketActivatedUnit(t, "zzsock2.service", sleepUnitContent())
	pid := waitForActive(t, u)
	fields := childEnv(t, pid)
	want := map[string]string{
		"LISTEN_PID":     strconv.Itoa(pid),
		"LISTEN_FDS":     "1",
		"LISTEN_FDNAMES": "zzsock2.socket",
	}
	for k, w := range want {
		if fields[k] != w {
			t.Errorf("child %s=%q, want %q", k, fields[k], w)
		}
	}
	if err := u.Stop(5 * time.Second); err != nil {
		t.Errorf("stop: %v", err)
	}
}

// socketActivatedUnit builds a unit holding one listener fd, exactly as the
// manager hands one over. The fd belongs to the unit from here: a start either
// uses it or releases it, which is what the descriptor-leak test pins down.
func socketActivatedUnit(t *testing.T, name, content string) *Unit {
	t.Helper()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "a.sock"), Net: "unix"})
	if err != nil {
		t.Skipf("no unix sockets here: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	fd, err := ln.File()
	if err != nil {
		t.Fatalf("listener fd: %v", err)
	}
	unitName := strings.TrimSuffix(name, ".service") + ".socket"
	u := newTestUnit(t, name, content)
	u.SetSocketActivation([]*os.File{fd}, map[string]string{
		"LISTEN_FDS": "1", "LISTEN_FDNAMES": unitName,
	})
	return u
}

func sleepUnitContent(directives ...string) string {
	body := "[Service]\nType=simple\nExecStart=/bin/sleep 30\n"
	for _, d := range directives {
		body += d + "\n"
	}
	return body
}

// procField is one line of /proc/<pid>/status, e.g. "Umask".
func procField(t *testing.T, pid int, field string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		t.Skipf("cannot read the child's status: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && k == field {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// UMask= (and LimitNOFILE=) are applied by wrapping the command in a shell of
// their own, and socket activation wraps it in another one for $LISTEN_PID. Two
// execs in a row keep the pid, but only if neither wrap forgets to exec: a
// forked inner shell would hand the daemon a $LISTEN_PID that is already gone
// and a umask nobody asked for.
func TestSocketActivationKeepsTheHardeningWrap(t *testing.T) {
	u := socketActivatedUnit(t, "zzumask.service", sleepUnitContent("UMask=0077"))
	pid := waitForActive(t, u)
	defer func() { _ = u.Stop(5 * time.Second) }()

	if got := procField(t, pid, "Umask"); got != "0077" {
		t.Errorf("child Umask=%q, want 0077 from the outer wrap", got)
	}
	if got := childEnv(t, pid)["LISTEN_PID"]; got != strconv.Itoa(pid) {
		t.Errorf("child LISTEN_PID=%q, want its own pid %d", got, pid)
	}
}

// childEnv is what the running process actually has, which is the only answer
// that means anything: the supervisor's intentions are not its environment.
func childEnv(t *testing.T, pid int) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		t.Skipf("cannot read the child environment: %v", err)
	}
	env := map[string]string{}
	for _, kv := range strings.Split(string(data), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

// openFDCount is how many descriptors this process holds right now.
func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("cannot count open descriptors: %v", err)
	}
	return len(entries)
}

// A start that gives up before the main exec still has to release the sockets
// it was handed. The manager polls every 100ms while a connection stays queued,
// and each round dups the listener again, so descriptors that outlive failed
// starts finish the whole daemon with EMFILE rather than leave one unit red in
// `systemctl status`.
func TestFailedActivationStartDoesNotLeakDescriptors(t *testing.T) {
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "leak.sock"), Net: "unix"})
	if err != nil {
		t.Skipf("no unix sockets here: %v", err)
	}
	defer func() { _ = ln.Close() }()
	u := newTestUnit(t, "zzleak.service",
		"[Service]\nType=notify\nExecStartPre=/bin/false\nExecStart=/bin/sleep 30\n")
	// A leaked *os.File is closed whenever the garbage collector gets to it, so
	// an unfixed build could pass this test by accident. Stop that from being
	// the reason it passes.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	before := openFDCount(t)
	for i := 0; i < 6; i++ {
		fd, err := ln.File()
		if err != nil {
			t.Fatalf("listener fd: %v", err)
		}
		u.SetSocketActivation([]*os.File{fd}, map[string]string{"LISTEN_FDS": "1", "LISTEN_FDNAMES": "zzleak.socket"})
		// Type=notify waits for the spawn outcome, and a failing ExecStartPre
		// reports one, so this round is over - and its descriptors are either
		// closed or provably still held - by the time it returns.
		if _, err := u.StartAndWait(true); err == nil {
			t.Fatal("a failing ExecStartPre started the unit anyway")
		}
	}
	// A slack of two: this is a live process, and other goroutines are allowed
	// to open things. Six rounds must not be six more descriptors.
	if grew := openFDCount(t) - before; grew > 2 {
		t.Errorf("%d descriptors left open by 6 failed socket-activated starts", grew)
	}
}
