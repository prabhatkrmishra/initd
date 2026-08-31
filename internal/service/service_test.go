package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"initd/internal/parser"
)

func newTestUnit(t *testing.T, name, content string) *Unit {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write unit: %v", err)
	}
	cfg, err := parser.ParseUnit(path)
	if err != nil {
		t.Fatalf("parse unit: %v", err)
	}
	return NewUnit(cfg, path)
}

func TestParseSystemdDuration(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", 5 * time.Second},
		{"0", 0},
		{"infinity", 0},
		{"INFINITY", 0},
		{"2s", 2 * time.Second},
		{"500ms", 500 * time.Millisecond},
		{"1m", time.Minute},
		{"30", 30 * time.Second}, // bare number treated as seconds
		{"garbage", 5 * time.Second},
	}
	for _, c := range cases {
		if got := parseSystemdDuration(c.raw, 5*time.Second); got != c.want {
			t.Errorf("parseSystemdDuration(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

func TestParseExitStatusSet(t *testing.T) {
	if got := parseExitStatusSet(""); got != nil {
		t.Errorf("parseExitStatusSet(\"\") = %v, want nil", got)
	}
	got := parseExitStatusSet("0 1 130")
	if len(got) != 3 {
		t.Fatalf("parseExitStatusSet = %v, want 3 entries", got)
	}
	for _, v := range []int{0, 1, 130} {
		if _, ok := got[v]; !ok {
			t.Errorf("missing exit status %d", v)
		}
	}
	// Non-numeric entries are skipped
	if got := parseExitStatusSet("0 abc 2"); len(got) != 2 {
		t.Errorf("parseExitStatusSet with junk = %v, want 2 entries", got)
	}
}

func TestParseFileMode(t *testing.T) {
	mode, err := parseFileMode("", 0o644)
	if err != nil || mode != 0o644 {
		t.Errorf("parseFileMode(\"\") = %v, %v; want 0644", mode, err)
	}
	mode, err = parseFileMode("0600", 0o644)
	if err != nil || mode != 0o600 {
		t.Errorf("parseFileMode(\"0600\") = %v, %v; want 0600", mode, err)
	}
	if _, err := parseFileMode("zzz", 0o644); err == nil {
		t.Error("parseFileMode(\"zzz\") should error")
	}
}

func TestExpandWithEnv(t *testing.T) {
	env := map[string]string{"FOO": "bar", "EMPTY": ""}
	if got := expandWithEnv("$FOO-$MISSING", env); got != "bar-" {
		t.Errorf("expandWithEnv = %q, want %q", got, "bar-")
	}
}

func TestStripPrefix(t *testing.T) {
	cmd, ignore := stripPrefix("/bin/true")
	if cmd != "/bin/true" || ignore {
		t.Errorf("stripPrefix(/bin/true) = %q, %v", cmd, ignore)
	}
	cmd, ignore = stripPrefix("-/bin/false")
	if cmd != "/bin/false" || !ignore {
		t.Errorf("stripPrefix(-/bin/false) = %q, %v", cmd, ignore)
	}
}

func TestCanonicalServiceType(t *testing.T) {
	cases := map[string]string{
		"":              "simple",
		"simple":        "simple",
		"SIMPLE":        "simple",
		"forking":       "forking",
		"oneshot":       "oneshot",
		"idle":          "idle",
		"exec":          "exec",
		"notify":        "notify",
		"notify-reload": "notify",
		"dbus":          "simple",
		"bogus":         "simple",
	}
	for in, want := range cases {
		u := newTestUnit(t, "x.service", "[Service]\nType="+in+"\nExecStart=/bin/true\n")
		if got := u.canonicalServiceType(); got != want {
			t.Errorf("canonicalServiceType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWorkingDirectory(t *testing.T) {
	// Default is "/" (systemd-compatible), not the unit file's directory.
	u := newTestUnit(t, "x.service", "[Service]\nExecStart=/bin/true\n")
	if got := u.workingDirectory(); got != "/" {
		t.Errorf("workingDirectory() default = %q, want /", got)
	}

	u2 := newTestUnit(t, "x.service", "[Service]\nExecStart=/bin/true\nWorkingDirectory=/srv/app\n")
	if got := u2.workingDirectory(); got != "/srv/app" {
		t.Errorf("workingDirectory() = %q, want /srv/app", got)
	}
}

func TestCommandExitStatus(t *testing.T) {
	// Exited with status 3: exit status lives in bits 8-15.
	ws := syscall.WaitStatus(3 << 8)
	if got := commandExitStatus(ws); got != 3 {
		t.Errorf("commandExitStatus(exited 3) = %d, want 3", got)
	}
	// Signaled with SIGTERM (15) -> 128+15 = 143
	ws = syscall.WaitStatus(syscall.SIGTERM)
	if got := commandExitStatus(ws); got != 143 {
		t.Errorf("commandExitStatus(SIGTERM) = %d, want 143", got)
	}
}

func TestWaitStatusError(t *testing.T) {
	if err := waitStatusError(syscall.WaitStatus(0)); err != nil {
		t.Errorf("waitStatusError(exit 0) = %v, want nil", err)
	}
	if err := waitStatusError(syscall.WaitStatus(2 << 8)); err == nil {
		t.Error("waitStatusError(exit 2) should error")
	}
	if err := waitStatusError(syscall.WaitStatus(syscall.SIGKILL)); err == nil {
		t.Error("waitStatusError(SIGKILL) should error")
	}
}

func TestStopRequestedFlag(t *testing.T) {
	u := newTestUnit(t, "x.service", "[Service]\nExecStart=/bin/true\n")
	if u.StopRequested() {
		t.Error("StopRequested() should be false initially")
	}
	// Start clears the flag
	if _, err := u.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if u.StopRequested() {
		t.Error("StopRequested() should be false after Start")
	}
	// Stop sets the flag
	if err := u.Stop(2 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !u.StopRequested() {
		t.Error("StopRequested() should be true after Stop")
	}
}

func TestRecordRestart(t *testing.T) {
	u := newTestUnit(t, "x.service", "[Service]\nExecStart=/bin/true\n")
	now := time.Now()
	if got := u.RecordRestart(now, 10*time.Second); got != 1 {
		t.Errorf("first RecordRestart = %d, want 1", got)
	}
	if got := u.RecordRestart(now.Add(2*time.Second), 10*time.Second); got != 2 {
		t.Errorf("second RecordRestart = %d, want 2", got)
	}
	// Old entries outside the window are pruned
	if got := u.RecordRestart(now.Add(20*time.Second), 10*time.Second); got != 1 {
		t.Errorf("RecordRestart after window = %d, want 1", got)
	}
}

func TestMarkFailedResetFailed(t *testing.T) {
	u := newTestUnit(t, "x.service", "[Service]\nExecStart=/bin/true\n")
	u.MarkFailed("boom")
	if !u.IsFailed() {
		t.Error("IsFailed() should be true after MarkFailed")
	}
	if u.Snapshot().LastError != "boom" {
		t.Errorf("LastError = %q, want boom", u.Snapshot().LastError)
	}
	if !u.ResetFailed() {
		t.Error("ResetFailed() should return true when it was failed")
	}
	if u.IsFailed() {
		t.Error("IsFailed() should be false after ResetFailed")
	}
}

func TestStopTimeoutStartTimeout(t *testing.T) {
	u := newTestUnit(t, "x.service", "[Service]\nExecStart=/bin/true\nTimeoutStopSec=7\nTimeoutStartSec=9\n")
	if got := u.StopTimeout(); got != 7*time.Second {
		t.Errorf("StopTimeout = %v, want 7s", got)
	}
	if got := u.StartTimeout(); got != 9*time.Second {
		t.Errorf("StartTimeout = %v, want 9s", got)
	}
	// Defaults
	u2 := newTestUnit(t, "x.service", "[Service]\nExecStart=/bin/true\n")
	if got := u2.StopTimeout(); got != 10*time.Second {
		t.Errorf("StopTimeout default = %v, want 10s", got)
	}
	if got := u2.StartTimeout(); got != 30*time.Second {
		t.Errorf("StartTimeout default = %v, want 30s", got)
	}
}

func TestSimpleServiceStartStop(t *testing.T) {
	u := newTestUnit(t, "sleep.service", "[Service]\nType=simple\nExecStart=/bin/sleep 30\n")
	if _, err := u.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Wait for it to become active
	deadline := time.Now().Add(3 * time.Second)
	for {
		if u.Snapshot().State == StateActive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("service did not become active, state=%s", u.Snapshot().State)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if u.Snapshot().MainPID == 0 {
		t.Error("MainPID should be set for active simple service")
	}
	if err := u.Stop(3 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := u.Snapshot().State; got != StateInactive {
		t.Errorf("state after stop = %s, want inactive", got)
	}
}

func TestOneshotService(t *testing.T) {
	u := newTestUnit(t, "true.service", "[Service]\nType=oneshot\nExecStart=/bin/true\n")
	if _, err := u.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Oneshot should complete and end inactive (success)
	deadline := time.Now().Add(3 * time.Second)
	for {
		state := u.Snapshot().State
		if state == StateInactive || state == StateFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("oneshot did not finish, state=%s", state)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := u.Snapshot().State; got != StateInactive {
		t.Errorf("oneshot state = %s, want inactive", got)
	}
}

func TestSimpleServiceFailure(t *testing.T) {
	u := newTestUnit(t, "false.service", "[Service]\nType=simple\nExecStart=/bin/false\n")
	if _, err := u.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if u.Snapshot().State == StateFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("service did not fail, state=%s", u.Snapshot().State)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if u.Snapshot().ExitCode == 0 {
		t.Error("ExitCode should be non-zero for /bin/false")
	}
}

// TestNotifyServiceExitsBeforeReady verifies that a Type=notify process that
// exits before sending READY=1 is reaped and the unit is marked failed, rather
// than being left stuck in "activating" or wrongly reported "active" because a
// zombie still passes processAlive. This exercises the reaper==nil path.
func TestNotifyServiceExitsBeforeReady(t *testing.T) {
	u := newTestUnit(t, "notify-false.service", "[Service]\nType=notify\nExecStart=/bin/false\n")
	if _, err := u.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		state := u.Snapshot().State
		if state == StateFailed || state == StateInactive {
			break
		}
		if state == StateActive {
			t.Fatalf("notify service whose process exited before READY was marked active")
		}
		if time.Now().After(deadline) {
			t.Fatalf("notify service did not settle, state=%s", state)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := u.Snapshot().State; got != StateFailed {
		t.Errorf("state = %s, want failed", got)
	}
	if u.Snapshot().ExitCode == 0 {
		t.Error("ExitCode should be non-zero for /bin/false")
	}
}

func TestProcessAliveZombie(t *testing.T) {
	// A zombie must not be reported as alive.
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	if !processAlive(pid) {
		t.Fatal("running process should be alive")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	// Wait for it to become a zombie (exited but not yet reaped).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if procState(pid) == 'Z' {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("process did not become a zombie")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Fatal("zombie should not be reported as alive")
	}
	// Reap it to avoid leaking a zombie.
	cmd.Process.Wait()
}

// procState reads the process state character from /proc/<pid>/stat.
func procState(pid int) byte {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	if idx := strings.LastIndexByte(string(data), ')'); idx >= 0 && idx+2 < len(data) {
		return data[idx+2]
	}
	return 0
}
