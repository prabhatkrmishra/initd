package service

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"initd/internal/userpaths"
)

// waitForState parks on a unit's asynchronous state transitions (the reaper
// and start goroutines report failures on their own schedule).
func waitForState(t *testing.T, u *Unit, want State) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if u.Snapshot().State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("unit stayed %q, want %q", u.Snapshot().State, want)
}

// Host systemd 259, verified: forking/notify/exec jobs are still open when
// the exec is refused, so the failure reaches `systemctl start`.
func TestStartAndWaitReportsExecFailure(t *testing.T) {
	u := newTestUnit(t, "zzbad.service", "[Service]\nType=notify\nExecStart=/nonexistent-initd-test-binary\n")
	if _, err := u.StartAndWait(true); err == nil {
		t.Fatal("StartAndWait() = nil for a binary that cannot be exec'd, want error")
	} else if !strings.Contains(err.Error(), "control process exited with error code") {
		t.Errorf("StartAndWait() error = %v, want systemd's job wording", err)
	}
	snap := u.Snapshot()
	if snap.State != StateFailed {
		t.Errorf("state after failed exec = %q, want failed", snap.State)
	}
	if snap.Result != "exit-code" {
		t.Errorf("Result = %q, want exit-code", snap.Result)
	}
	if snap.ExitCode != 203 {
		t.Errorf("ExitCode = %d, want 203 (EXEC)", snap.ExitCode)
	}
	if !strings.Contains(snap.LastError, "nonexistent-initd-test-binary") {
		t.Errorf("LastError = %q, want the reason kept for status output", snap.LastError)
	}
}

// Host systemd 259, verified: a unit whose ConditionPathExists= is not met is
// NOT failed. The start is skipped, `systemctl start` answers 0 and the unit
// reads inactive (dead) with a success result, exactly like one never started.
func TestUnmetConditionSkipsStartNotFails(t *testing.T) {
	u := newTestUnit(t, "zzcond.service", "[Unit]\nConditionPathExists=/nonexistent-initd-condition\n[Service]\nExecStart=/bin/sleep 30\n")
	if _, err := u.StartAndWait(true); err != nil {
		t.Fatalf("StartAndWait() = %v for an unmet condition, want the skip reported as success", err)
	}
	snap := u.Snapshot()
	if snap.State == StateFailed {
		t.Errorf("state after unmet condition = %q, want not failed", snap.State)
	}
	if snap.State != StateInactive {
		t.Errorf("state after unmet condition = %q, want inactive", snap.State)
	}
	if snap.Result != "success" {
		t.Errorf("Result = %q, want success", snap.Result)
	}
	if snap.MainPID != 0 {
		t.Errorf("MainPID = %d, want 0 (never spawned)", snap.MainPID)
	}
}

// systemd's Type=simple job completes at fork, so a later exec refusal is NOT
// reported to the caller: `systemctl start` answers 0 and the unit shows
// status=203/EXEC. Faithfulness matters more than the nicety here - scripts
// written for systemd count on this.
func TestStartAndWaitSimpleReportsForkNotExec(t *testing.T) {
	u := newTestUnit(t, "zzsimple.service", "[Service]\nType=simple\nExecStart=/nonexistent-initd-test-binary\n")
	if _, err := u.StartAndWait(true); err != nil {
		t.Errorf("StartAndWait(simple) = %v, want nil because the job ended at fork", err)
	}
	waitForState(t, u, StateFailed)
	snap := u.Snapshot()
	if snap.ExitCode != 203 {
		t.Errorf("ExecMainStatus = %d, want 203 (EXEC)", snap.ExitCode)
	}
	if snap.Result != "exit-code" {
		t.Errorf("Result = %q, want exit-code", snap.Result)
	}
}

// A oneshot job waits for the control process, so a plain non-zero exit is a
// failed start too.
func TestStartAndWaitOneshotReportsExitCode(t *testing.T) {
	u := newTestUnit(t, "zzoneshot.service", "[Service]\nType=oneshot\nExecStart=/bin/false\n")
	_, err := u.StartAndWait(true)
	if err == nil {
		t.Fatal("StartAndWait(oneshot /bin/false) = nil, want a failed job")
	}
	if !strings.Contains(err.Error(), "control process exited with error code") {
		t.Errorf("error = %q, want systemd's job wording", err)
	}
	if snap := u.Snapshot(); snap.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", snap.ExitCode)
	}
}

func TestStartAndWaitNotifyWaitsForReadiness(t *testing.T) {
	// A Type=notify job is only done when the service says READY=1, so a unit
	// that never signals has to fail at TimeoutStartSec - returning success
	// over a unit still in "activating" is what let a broken notify protocol
	// look healthy, and made `start && curl` race the service.
	u := newTestUnit(t, "zznotify.service", "[Service]\nType=notify\nTimeoutStartSec=1\nExecStart=/bin/sleep 30\n")
	defer func() { _ = u.Stop(2 * time.Second) }()
	started := time.Now()
	_, err := u.StartAndWait(true)
	if err == nil {
		t.Fatal("StartAndWait() = nil for a notify unit that never signals ready")
	}
	if !strings.Contains(err.Error(), "a timeout was exceeded") {
		t.Errorf("error = %q, want systemd's timeout wording", err)
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
		t.Errorf("StartAndWait() returned after %v, want it to wait out TimeoutStartSec", elapsed)
	}
	if snap := u.Snapshot(); snap.Result != "timeout" {
		t.Errorf("Result = %q, want timeout", snap.Result)
	}
	// Upstream records the timeout kill as a signaled death: code=killed with
	// the stop signal as the status, and the process keeps its ExecMainPID so
	// `systemctl status` can still print the Process: line for it.
	snap := u.Snapshot()
	if snap.MainCode != 2 {
		t.Errorf("MainCode = %d, want 2 (killed)", snap.MainCode)
	}
	if snap.ExitCode != int(syscall.SIGTERM) {
		t.Errorf("ExitCode = %d, want %d (SIGTERM)", snap.ExitCode, int(syscall.SIGTERM))
	}
	if snap.ExecMainPID <= 0 {
		t.Errorf("ExecMainPID = %d, want the spawned pid retained for the status line", snap.ExecMainPID)
	}
}

func TestStartAndWaitToleratedExecFailure(t *testing.T) {
	u := newTestUnit(t, "zzdash.service", "[Service]\nType=notify\nExecStart=-/nonexistent-initd-test-binary\n")
	if _, err := u.StartAndWait(true); err != nil {
		t.Errorf("StartAndWait() = %v for a -prefixed ExecStart, want nil", err)
	}
	if snap := u.Snapshot(); snap.State == StateFailed {
		t.Error("unit with -prefixed ExecStart ended failed, want inactive")
	}
}

func TestStartAndWaitConcurrentStartsKeepTheirOwnOutcome(t *testing.T) {
	// Two starts of the same unit: the second sees it activating and returns
	// the running token. Neither waiter may consume the other's outcome.
	u := newTestUnit(t, "zztwice.service", "[Service]\nType=simple\nExecStart=/bin/sleep 30\n")
	defer func() { _ = u.Stop(2 * time.Second) }()
	first, err := u.StartAndWait(true)
	if err != nil {
		t.Fatalf("first StartAndWait() = %v", err)
	}
	second, err := u.StartAndWait(true)
	if err != nil {
		t.Fatalf("second StartAndWait() = %v", err)
	}
	if first != second {
		t.Errorf("tokens = %d then %d, want the same unit start reported twice", first, second)
	}
}

// systemd substitutes a plain $NAME/${NAME} and hands every other dollar
// construct to the child shell. Swallowing `${VAR:-default}` (Go's os.Expand
// reads the whole brace body as the name) deleted default values from
// command lines, which is how ${XDG_RUNTIME_DIR:-none} ran as an empty path.
// A signaled death arrives through exec.ExitError too; before it was routed
// by wait status, status.ExitStatus() returned -1 for those and the unit
// reported a nonsensical exit code.
func TestSignalDeathReportsKilledCode(t *testing.T) {
	u := newTestUnit(t, "zzkill.service", "[Service]\nType=simple\nExecStart=/bin/sleep 30\n")
	if _, err := u.StartAndWait(true); err != nil {
		t.Fatalf("StartAndWait: %v", err)
	}
	waitForState(t, u, StateActive)
	pid := u.Snapshot().MainPID
	if pid <= 0 {
		t.Fatal("no MainPID recorded for a running unit")
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}
	waitForState(t, u, StateFailed)
	snap := u.Snapshot()
	if snap.ExitCode != 137 {
		t.Errorf("ExitCode = %d, want 137 (128+SIGKILL)", snap.ExitCode)
	}
	if snap.MainCode != 2 {
		t.Errorf("MainCode = %d, want 2 (killed)", snap.MainCode)
	}
	if snap.Result != "exit-code" {
		t.Errorf("Result = %q, want exit-code", snap.Result)
	}
	if snap.FinishedAt.IsZero() {
		t.Error("FinishedAt not stamped when the process was reaped")
	}
}

func TestExpandWithEnvLeavesShellConstructs(t *testing.T) {
	env := map[string]string{"FOO": "bar"}
	cases := []struct{ in, want string }{
		{"$FOO", "bar"},
		{"${FOO}", "bar"},
		{"$MISSING", ""},
		{"${MISSING}", ""},
		{"${MISSING:-fallback}", "${MISSING:-fallback}"},
		{"${FOO:-fallback}", "${FOO:-fallback}"},
		{"${FOO#b}", "${FOO#b}"},
		{"$1", "$1"},
		{"$", "$"},
		{"$$", "$"},
		{"${UNTERMINATED", "${UNTERMINATED"},
		{"echo ${XDG_RUNTIME_DIR:-none} > $FOO/out", "echo ${XDG_RUNTIME_DIR:-none} > bar/out"},
	}
	for _, c := range cases {
		if got := expandWithEnv(c.in, env); got != c.want {
			t.Errorf("expandWithEnv(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExpandSpecifiersRuntimeAndStateDirs(t *testing.T) {
	u := newTestUnit(t, "zzspec@inst.service", "[Service]\nExecStart=/bin/true\n")
	run, state, cache, logs, _ := u.directoryBases()
	got := u.expandSpecifiers("%t/%S/%C/%L")
	want := strings.Join([]string{run, state, cache, logs}, "/")
	if got != want {
		t.Errorf("expanded directory specifiers = %q, want %q", got, want)
	}
	if got := u.expandSpecifiers("100%%"); got != "100%" {
		t.Errorf("%%%% handling = %q, want 100%%", got)
	}
	if got := u.expandSpecifiers("%i-%p"); got != "inst-zzspec" {
		t.Errorf("instance specifiers = %q, want inst-zzspec", got)
	}
	// systemd 259: for a unit that is not a template instance, %p and %N are
	// both the bare name.
	plain := newTestUnit(t, "plain.service", "[Service]\nExecStart=/bin/true\n")
	if got := plain.expandSpecifiers("%n|%N|%p|%i|"); got != "plain.service|plain|plain||" {
		t.Errorf("non-instanced specifiers = %q, want plain.service|plain|plain||", got)
	}
}

func TestUnitEnvironmentHasRuntimeDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("system scope does not export XDG_RUNTIME_DIR")
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	u := newTestUnit(t, "zzenv.service", "[Service]\nExecStart=/bin/true\n")
	envMap, _, err := u.buildEnvironment()
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	if got := envMap["XDG_RUNTIME_DIR"]; got != userpaths.UserRuntimeDir() {
		t.Errorf("XDG_RUNTIME_DIR (set-but-empty) = %q, want %q", got, userpaths.UserRuntimeDir())
	}
	os.Unsetenv("XDG_RUNTIME_DIR")
	envMap, _, err = u.buildEnvironment()
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	if got := envMap["XDG_RUNTIME_DIR"]; got != userpaths.UserRuntimeDir() {
		t.Errorf("XDG_RUNTIME_DIR (unset) = %q, want %q", got, userpaths.UserRuntimeDir())
	}
	// An explicit Environment= still wins over the injected default.
	u2 := newTestUnit(t, "zzenv2.service", "[Service]\nExecStart=/bin/true\nEnvironment=XDG_RUNTIME_DIR=/elsewhere\n")
	envMap2, _, err := u2.buildEnvironment()
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	if got := envMap2["XDG_RUNTIME_DIR"]; got != "/elsewhere" {
		t.Errorf("explicit XDG_RUNTIME_DIR = %q, want /elsewhere", got)
	}
}

// Host systemd 259, verified: `ExecStart=-/bin/false` reports a successful,
// dead unit with no trace of the exit - ExecMainCode=0 ExecMainStatus=0.
// Carrying the status through made `systemctl show` claim a tolerated failure
// had recorded status=1 while Result said success.
func TestToleratedExitLeavesNoFailureRecorded(t *testing.T) {
	u := newTestUnit(t, "zztol.service", "[Service]\nType=oneshot\nExecStart=-/bin/false\n")
	if _, err := u.StartAndWait(true); err != nil {
		t.Fatalf("StartAndWait(-/bin/false) = %v, want the tolerated failure reported as success", err)
	}
	waitForState(t, u, StateInactive)
	snap := u.Snapshot()
	if snap.MainCode != 0 || snap.ExitCode != 0 {
		t.Errorf("ExecMainCode/ExecMainStatus = %d/%d, want 0/0", snap.MainCode, snap.ExitCode)
	}
	if snap.Result != "success" {
		t.Errorf("Result = %q, want success", snap.Result)
	}
	if snap.LastError != "" {
		t.Errorf("LastError = %q, want empty", snap.LastError)
	}
	if u.IsFailed() {
		t.Error("unit is failed after a tolerated exit")
	}
}
