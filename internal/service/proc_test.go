package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// writeProcFixture lays out a directory that looks like /proc to the scans:
// one numeric directory per process, each with the status and cmdline the
// kernel would provide. pids here are make-believe, so these tests only run
// the file-reading half of the logic - which is the half that decides
// ownership.
func writeProcFixture(t *testing.T, entries map[uint32][]int) string {
	t.Helper()
	root := t.TempDir()
	for uid, pids := range entries {
		for _, pid := range pids {
			dir := filepath.Join(root, fmt.Sprint(pid))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("fixture dir: %v", err)
			}
			status := fmt.Sprintf("Name:\tzzprobe\nState:\tS (sleeping)\nTgid:\t%d\nPid:\t%d\nUid:\t%d\t%d\t%d\t%d\nGid:\t0\t0\t0\t0\n",
				pid, pid, uid, uid, uid, uid)
			if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o444); err != nil {
				t.Fatalf("fixture status: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, "cmdline"),
				[]byte("/bin/sleep\x00421\x00"), 0o444); err != nil {
				t.Fatalf("fixture cmdline: %v", err)
			}
		}
	}
	t.Setenv("INITD_PROC_ROOT", root)
	return root
}

const fixtureArg = "421"

func TestFindExternalPIDIgnoresProcessesOfOtherUsers(t *testing.T) {
	self := uint32(os.Getuid())
	other := uint32(65534)
	if other == self {
		t.Skip("the nobody uid is this user's uid")
	}
	writeProcFixture(t, map[uint32][]int{
		other: {4242},
		self:  {4243},
	})

	u := newTestUnit(t, "zzext.service", "[Service]\nType=simple\nExecStart=/bin/sleep "+fixtureArg+"\n")
	pid, cmdline := u.FindExternalPID()
	if pid != 4243 {
		t.Fatalf("FindExternalPID() = %d (%q), want 4243: the only sleep this unit could own", pid, cmdline)
	}
}

func TestFindExternalPIDRejectsInvisibleProcess(t *testing.T) {
	// An entry with no readable status is exactly what hidepid=2 leaves of
	// another user's process. It must never be adopted: the daemon cannot
	// signal what it cannot see.
	root := t.TempDir()
	dir := filepath.Join(root, "4244")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("fixture dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte("/bin/sleep\x00"+fixtureArg+"\x00"), 0o444); err != nil {
		t.Fatalf("fixture cmdline: %v", err)
	}
	t.Setenv("INITD_PROC_ROOT", root)

	u := newTestUnit(t, "zzext2.service", "[Service]\nType=simple\nExecStart=/bin/sleep "+fixtureArg+"\n")
	if pid, _ := u.FindExternalPID(); pid != 0 {
		t.Fatalf("FindExternalPID() = %d, want 0: an unreadable owner is not ours", pid)
	}
}

func TestProcUIDsAndOwnedBy(t *testing.T) {
	writeProcFixture(t, map[uint32][]int{1234: {4245}})
	if _, _, ok := procUIDs(4245); !ok {
		t.Fatal("procUIDs(4245) reported the entry unreadable")
	}
	if real, eff, _ := procUIDs(4245); real != 1234 || eff != 1234 {
		t.Errorf("procUIDs(4245) = %d/%d, want 1234/1234", real, eff)
	}
	if !procOwnedBy(4245, 1234) {
		t.Error("procOwnedBy(4245, 1234) = false, want true")
	}
	if procOwnedBy(4245, 1235) {
		t.Error("procOwnedBy(4245, 1235) = true, want false")
	}
	if procOwnedBy(9999, 1234) {
		t.Error("procOwnedBy on a missing entry = true, want false")
	}
}

func TestCheckLivenessTriState(t *testing.T) {
	if got := checkLiveness(0); got != livenessDead {
		t.Errorf("checkLiveness(0) = %v, want dead", got)
	}
	if got := checkLiveness(1 << 22); got != livenessDead {
		t.Errorf("checkLiveness(no such pid) = %v, want dead", got)
	}
	if got := checkLiveness(os.Getpid()); got != livenessAlive {
		t.Errorf("checkLiveness(self) = %v, want alive", got)
	}
	// A live pid whose /proc entry is hidden stays alive: kill() accepted the
	// signal and nothing here proved the process went away.
	cmd := exec.Command("/bin/sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Skip("sleep unavailable")
	}
	defer cmd.Process.Kill()
	t.Setenv("INITD_PROC_ROOT", t.TempDir())
	if got := checkLiveness(cmd.Process.Pid); got != livenessAlive {
		t.Fatalf("checkLiveness with an unreadable /proc entry = %v, want alive", got)
	}
}

func TestGroupHasOwnedMemberIgnoresEntriesTheKernelDenies(t *testing.T) {
	if syscall.Kill(4246, 0) == nil {
		t.Skip("pid 4246 is a real process here")
	}
	writeProcFixture(t, map[uint32][]int{uint32(os.Getuid()): {4246}})
	// 4246 exists only in the fixture, so the real Getpgid answers ESRCH and
	// no group counts as populated: a directory listing is not proof of life.
	if groupHasOwnedMember(4246, uint32(os.Getuid())) {
		t.Fatal("groupHasOwnedMember trusted a /proc entry that the kernel says is gone")
	}
}

// The device runs the daemon where hidepid=2 hides every process it does not
// own, and root's initd owns a copy of the same unit tree. A recorded group ID
// is only a number, and once our starter exits the kernel can hand it to a
// group whose members are invisible to us - so the sweep must be gated on the
// scan finding one of ours, not on the number having been recorded.
func TestGroupKillSkipsGroupWhoseMembersAreHidden(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "20")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skip("sleep unavailable")
	}
	foreignPGID := cmd.Process.Pid
	defer func() {
		_ = syscall.Kill(-foreignPGID, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := syscall.Getpgid(foreignPGID); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the child never joined a process group")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(-foreignPGID, 0); err != nil {
		t.Fatalf("setup: the group should exist and be signal-able, got %v", err)
	}

	// Stand in for hidepid: the scan can no longer see anything, while the
	// group is very much alive.
	t.Setenv("INITD_PROC_ROOT", t.TempDir())
	u := mkUnit("zzrecycled.service", "control-group")
	// The orphan case: no main PID, only the recorded group.
	u.signalUnitPID(0, foreignPGID, true, syscall.SIGTERM)
	if err := syscall.Kill(-foreignPGID, 0); err != nil {
		t.Fatalf("the sweep reached a group the scan found no member of: %v", err)
	}
}

func TestPIDFileNamingAnUnsignalableProcessIsNotAdopted(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("as root, PID 1 can be signalled, so it is not this case")
	}
	file := filepath.Join(t.TempDir(), "daemon.pid")
	if err := os.WriteFile(file, []byte("1\n"), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
	u := newTestUnit(t, "zzpidfile.service",
		"[Service]\nType=forking\nExecStart=/bin/true\nPIDFile="+file+"\n")

	// PID 1 exists, and kill(0) answers EPERM: a main PID we can neither
	// signal nor reap could never be stopped, so `status` pointing init at it
	// was a dead end rather than a supervised service.
	if checkLiveness(1) != livenessUnknown {
		t.Skip("PID 1 is not in the exists-but-unsingalable state here")
	}
	if pid := u.livePIDFilePID(); pid != 0 {
		t.Errorf("livePIDFilePID() = %d, want 0: PID 1 is not ours to supervise", pid)
	}
}

// The stale-pid-file hazard: the daemon died, and the kernel handed its number
// to an unrelated process. The number alone cannot tell the two apart, but a
// process that began after the file was last written cannot have written it.
func TestStalePIDFileNumberIsNotAdopted(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Skip("sleep unavailable")
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	dir := t.TempDir()
	file := filepath.Join(dir, "daemon.pid")
	if err := os.WriteFile(file, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
	unit := func() *Unit {
		return newTestUnit(t, "zzstale.service",
			"[Service]\nType=forking\nExecStart=/bin/true\nPIDFile="+file+"\n")
	}

	stale := time.Now().Add(-time.Hour)
	if err := os.Chtimes(file, stale, stale); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if pid := unit().livePIDFilePID(); pid != 0 {
		t.Errorf("livePIDFilePID() = %d for a file written an hour before the process, want 0", pid)
	}

	// A disagreement the size of a clock step is not evidence: /proc start
	// times are rebuilt from the boot time, so a forward jump moves them away
	// from an mtime written before it and would refuse a daemon that really
	// did write its own file.
	almostNow := time.Now().Add(-30 * time.Second)
	if err := os.Chtimes(file, almostNow, almostNow); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if pid := unit().livePIDFilePID(); pid != cmd.Process.Pid {
		t.Errorf("livePIDFilePID() = %d for a file 30s out of step, want %d adopted", pid, cmd.Process.Pid)
	}

	// The same process, named by a file it could have written: adopted, so the
	// guard does not break the Type=forking services that rely on PIDFile=.
	now := time.Now()
	if err := os.Chtimes(file, now, now); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if pid := unit().livePIDFilePID(); pid != cmd.Process.Pid {
		t.Errorf("livePIDFilePID() = %d, want the running process %d", pid, cmd.Process.Pid)
	}
}

func TestExpectedUIDFollowsUserDirective(t *testing.T) {
	u := newTestUnit(t, "zzuid.service", "[Service]\nExecStart=/bin/true\n")
	if got := u.expectedUID(); got != uint32(os.Getuid()) {
		t.Errorf("expectedUID() without User= = %d, want the daemon's own %d", got, os.Getuid())
	}

	// A name the passwd database cannot resolve is not this unit's process
	// owner, and the unit cannot start either; the memo must not poison the
	// answer for the next config.
	u2 := newTestUnit(t, "zzuid2.service", "[Service]\nUser=zzno-such-user-initd\nExecStart=/bin/true\n")
	if got := u2.expectedUID(); got != uint32(os.Getuid()) {
		t.Errorf("expectedUID() with an unknown user = %d, want a fall back to %d", got, os.Getuid())
	}
}
