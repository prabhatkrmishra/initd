package service

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"initd/internal/cgroup"
)

// TestMain parks the suite's default tree in a temporary directory. Starting a
// unit makes a group for it, and a test that kills the process behind the
// daemon's back would otherwise leave that group behind on the host's real
// tree; the opt-in tests below reset the seam and ask the kernel themselves.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "initd-cgroup-suite")
	if err != nil {
		panic(err)
	}
	os.Setenv("INITD_CGROUP_ROOT", filepath.Join(dir, "initd"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// stubCgroups points the unit code at a tree over a temporary directory. The
// kernel creates cgroup.procs with the group and a temporary directory does
// not, so a test that needs a group calls leafFile first.
func stubCgroups(t *testing.T) *cgroup.Tree {
	t.Helper()
	root := t.TempDir()
	t.Setenv("INITD_CGROUP_ROOT", filepath.Join(root, "initd"))
	tree := cgroup.New()
	if !tree.Available() {
		t.Fatalf("stub tree under %s should be usable: %s", root, tree.Reason())
	}
	prev := cgroupTree
	cgroupTree = func() *cgroup.Tree { return tree }
	t.Cleanup(func() { cgroupTree = prev })
	return tree
}

// unavailableCgroups reproduces a box where the cgroup prefix exists but is not
// ours to write: every cgroup question must then answer "no evidence".
func unavailableCgroups(t *testing.T) {
	t.Helper()
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INITD_CGROUP_ROOT", filepath.Join(blocked, "initd"))
	prev := cgroupTree
	cgroupTree = func() *cgroup.Tree { return cgroup.New() }
	t.Cleanup(func() { cgroupTree = prev })
}

// leafFile prepares the unit's group and adds the file the kernel creates with
// it, without which nothing can be recorded as a member.
func leafFile(t *testing.T, u *Unit) string {
	t.Helper()
	if !u.prepareCgroup() {
		t.Fatal("prepareCgroup refused to create a group")
	}
	path, err := cgroupTree().LeafPath(u.cgroupID())
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(path, "cgroup.procs")
	if _, statErr := os.Stat(file); os.IsNotExist(statErr) {
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return file
}

func setMembers(t *testing.T, file string, pids ...int) {
	t.Helper()
	var b strings.Builder
	for _, pid := range pids {
		b.WriteString(strconv.Itoa(pid))
		b.WriteString("\n")
	}
	if err := os.WriteFile(file, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// spawn is a real process: liveness, start times and signalling all have to be
// decided about something the kernel knows about, not a made-up number.
func spawn(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start %v: %v", args, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

func waitForDeath(t *testing.T, pid int, context string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("%s is still running after 3s", context)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// kernelGroup is the group the cgroup2 line of a process's own cgroup file
// names. A v1 line carries a controller between its colons and would otherwise
// be read as agreement about a hierarchy this code does not use.
func kernelGroup(t *testing.T, pid int) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(procRoot(), strconv.Itoa(pid), "cgroup"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.SplitN(line, ":", 3)
		if len(f) == 3 && f[0] == "0" && f[1] == "" {
			return f[2]
		}
	}
	t.Fatalf("pid %d has no unified hierarchy entry: %q", pid, strings.TrimSpace(string(data)))
	return ""
}

func TestPlaceInCgroupClaimsTheStartedProcess(t *testing.T) {
	stubCgroups(t)
	u := mkUnit("claim.service", "control-group")
	file := leafFile(t, u)
	child := spawn(t, "/bin/sleep", "60")
	setMembers(t, file)
	u.placeInCgroup(child.Process.Pid)
	got := u.CGroupPIDs()
	if len(got) != 1 || got[0] != child.Process.Pid {
		t.Fatalf("the group holds %v, want the process this start made", got)
	}
	cg := u.ControlGroup()
	if !strings.HasPrefix(cg, "/") || !strings.HasSuffix(cg, "/claim.service") {
		t.Fatalf("ControlGroup() = %q", cg)
	}
}

// A process that outlived the process group it was started in is invisible to a
// group sweep, and is exactly what KillMode=control-group is for. The unit's
// recorded group is deliberately this test's own: the process-group path
// refuses to signal it, so only a membership kill can reach the member - and
// only a correct one can leave the caller alive.
func TestGroupKillReachesMembersThatLeftTheProcessGroup(t *testing.T) {
	stubCgroups(t)
	u := mkUnit("sweep.service", "control-group")
	file := leafFile(t, u)
	member := spawn(t, "/bin/sleep", "60")
	setMembers(t, file, member.Process.Pid)
	u.mu.Lock()
	own := syscall.Getpgrp()
	u.pgid = own
	u.mu.Unlock()

	u.signalUnitPID(0, own, true, syscall.SIGKILL)

	waitForDeath(t, member.Process.Pid, "the unit's group member")
	if !processAlive(os.Getpid()) {
		t.Fatal("the supervisor must never be signalled by a unit's group kill")
	}
}

// KillMode=mixed means the main process is asked politely and everything left
// behind is killed; KillMode=process means the children are none of our
// business. Both used to share a second half that never happened.
func TestStopSweepFollowsKillMode(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		wantDie bool
	}{
		{"mixed", true},
		{"control-group", true},
		{"process", false},
		{"none", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			stubCgroups(t)
			u := mkUnit("leftover.service", tc.mode)
			file := leafFile(t, u)
			leftover := spawn(t, "/bin/sleep", "60")
			pid := leftover.Process.Pid
			setMembers(t, file, pid)

			u.finishCgroupStop()

			if tc.wantDie {
				waitForDeath(t, pid, "a leftover KillMode="+tc.mode+" says to kill")
				if alive, known := u.cgroupAlive(); known && alive {
					t.Fatal("the group still reports the killed process as live")
				}
				return
			}
			if !processAlive(pid) {
				t.Fatalf("KillMode=%s killed a child it must leave alone", tc.mode)
			}
			// A survivor keeps the group: the kernel will not remove one that
			// still has a member, and pretending otherwise would lose track of
			// a process this unit started.
			if !u.hasCgroupLeaf() {
				t.Fatalf("the group was released while %d still runs in it", pid)
			}
		})
	}
}

func TestGroupMemberPIDIgnoresASurvivorFromTheLastStart(t *testing.T) {
	stubCgroups(t)
	u := mkUnit("adopt.service", "control-group")
	file := leafFile(t, u)
	stale := spawn(t, "/bin/sleep", "60")
	time.Sleep(1200 * time.Millisecond)
	fresh := spawn(t, "/bin/sleep", "60")

	u.mu.Lock()
	u.Runtime.StartedAt = time.Now().Add(-time.Second)
	u.mu.Unlock()
	setMembers(t, file, stale.Process.Pid, fresh.Process.Pid)

	if pid := u.cgroupMemberPID(0); pid != fresh.Process.Pid {
		t.Fatalf("cgroupMemberPID() = %d, want the process this start made (%d), not the survivor (%d)",
			pid, fresh.Process.Pid, stale.Process.Pid)
	}
	// Nothing left from this start: the caller must fall back rather than adopt
	// a process that predates it.
	u.mu.Lock()
	u.Runtime.StartedAt = time.Now().Add(time.Hour)
	u.mu.Unlock()
	if pid := u.cgroupMemberPID(0); pid != 0 {
		t.Fatalf("cgroupMemberPID() adopted an older process: %d", pid)
	}
}

// The temporary-directory tests above cannot prove the chain end to end: only
// the kernel refuses to move a process into a group it does not own, and only
// the kernel keeps a group alive while one of its members runs. These tests run
// the real thing and are skipped unless INITD_CGROUP_REAL=1 asks for it, which
// is how the arm64 test binary is verified on the chroot device.
func useRealTree(t *testing.T) {
	t.Helper()
	if os.Getenv("INITD_CGROUP_REAL") != "1" {
		t.Skip("set INITD_CGROUP_REAL=1 to exercise the kernel's cgroup tree")
	}
	prev := cgroupTree
	// A fresh discovery rather than cgroup.Default: the daemon's tree is cached
	// for its whole life, and a test must not depend on which unit ran first.
	cgroupTree = cgroup.New
	t.Cleanup(func() { cgroupTree = prev })
	t.Setenv("INITD_CGROUP_ROOT", "")
	if !cgroupTree().Available() {
		t.Fatalf("asked for the real tree, but this box has no writable cgroup prefix: %s", cgroupTree().Reason())
	}
}

func TestPlacementAndSweepAgainstTheRealTree(t *testing.T) {
	useRealTree(t)
	u := mkUnit("initd-selftest.service", "control-group")
	t.Cleanup(func() { _ = cgroupTree().Remove(u.cgroupID()) })
	if !u.prepareCgroup() {
		t.Fatal("no group could be created")
	}
	child := spawn(t, "/bin/sleep", "60")
	if err := cgroupTree().Attach(u.cgroupID(), child.Process.Pid); err != nil {
		t.Fatalf("the kernel refused the move: %v", err)
	}
	if !u.hasCgroupLeaf() {
		t.Fatal("the group prepareCgroup made is no longer recorded")
	}
	got := u.CGroupPIDs()
	if len(got) != 1 || got[0] != child.Process.Pid {
		t.Fatalf("CGroupPIDs() = %v, want the attached process", got)
	}
	// The kernel's own answer about where a process runs has to agree.
	cg, in := u.ControlGroup(), kernelGroup(t, child.Process.Pid)
	if cg == "" || cg != in {
		t.Fatalf("the child runs in %q, ControlGroup() says %q", in, cg)
	}
	if alive, known := u.cgroupAlive(); !alive || !known {
		t.Fatalf("cgroupAlive() = %v, %v with a live member", alive, known)
	}

	u.finishCgroupStop()
	waitForDeath(t, child.Process.Pid, "the member a control-group stop sweeps")
	if u.hasCgroupLeaf() {
		t.Fatal("the group survived the stop that emptied it")
	}
	if cg := u.ControlGroup(); cg != "" {
		t.Fatalf("ControlGroup() = %q after the group was removed", cg)
	}
}

// Whether the kernel gave the group back is not a question a temporary
// directory can answer: it is the kernel that refuses to remove a group with a
// member in it, and the kernel that deletes the directory when the last one
// goes.
func TestDeathReleasesTheGroupAgainstTheRealTree(t *testing.T) {
	useRealTree(t)
	u := mkUnit("initd-deadtest.service", "control-group")
	t.Cleanup(func() { _ = cgroupTree().Remove(u.cgroupID()) })
	if !u.prepareCgroup() {
		t.Fatal("no group could be created")
	}
	leaf, err := cgroupTree().LeafPath(u.cgroupID())
	if err != nil {
		t.Fatal(err)
	}
	child := spawn(t, "/bin/sleep", "60")
	if err := cgroupTree().Attach(u.cgroupID(), child.Process.Pid); err != nil {
		t.Fatalf("the kernel refused the move: %v", err)
	}

	// Killed behind the daemon's back, with no stop requested: the same shape as
	// a process that crashes.
	if err := syscall.Kill(child.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitForDeath(t, child.Process.Pid, "the member killed from outside")
	// The daemon is what reaps a unit's process, and the kernel charges a zombie
	// to its group until it is: unreaped, the rmdir below is refused no matter
	// how the release code is written.
	if err := child.Wait(); err != nil && !strings.Contains(err.Error(), "signal") {
		t.Fatalf("reaping the member: %v", err)
	}
	u.transitionState(StateFailed, "killed")
	if _, statErr := os.Stat(leaf); !os.IsNotExist(statErr) {
		t.Fatalf("the kernel still has the group the unit died with: %v", statErr)
	}
	if u.hasCgroupLeaf() {
		t.Fatal("the unit still believes it has a group")
	}
	if cg := u.ControlGroup(); cg != "" {
		t.Fatalf("ControlGroup() = %q for a unit that is gone", cg)
	}
}

// A start that times out is killed by the very path that then releases its
// group, and a signal is not a death: the process leaves the group only once the
// kernel gets around to running it. A release that looks once at that instant
// finds somebody home and hands nothing back, and nothing else ever looks
// again. The child here takes a moment to die, which is the whole point.
func TestATimeoutReleasesTheGroupAfterItsOwnKillAgainstTheRealTree(t *testing.T) {
	useRealTree(t)
	u := mkUnit("initd-timedout.service", "control-group")
	t.Cleanup(func() { _ = cgroupTree().Remove(u.cgroupID()) })
	if !u.prepareCgroup() {
		t.Fatal("no group could be created")
	}
	leaf, err := cgroupTree().LeafPath(u.cgroupID())
	if err != nil {
		t.Fatal(err)
	}
	// A shell parked in a foreground sleep does not run its trap until that
	// sleep finishes, so this one stays busy and dies when it is told to.
	ready := filepath.Join(t.TempDir(), "dying")
	child := spawn(t, "/bin/sh", "-c", `trap "sleep 0.1; exit 0" TERM; echo ready > "$1"; while :; do :; done`, "sh", ready)
	waitForFile(t, ready)
	if err := cgroupTree().Attach(u.cgroupID(), child.Process.Pid); err != nil {
		t.Fatalf("the kernel refused the move: %v", err)
	}
	u.mu.Lock()
	u.Runtime.State = StateActivating
	u.Runtime.MainPID = child.Process.Pid
	u.mu.Unlock()

	// Kill and release back to back, exactly as the timeout path does it.
	if err := syscall.Kill(child.Process.Pid, u.stopSignal()); err != nil {
		t.Fatal(err)
	}
	u.markTimeout(errors.New("notify timeout"), false)
	if snap := u.Snapshot(); snap.Result != "timeout" {
		t.Fatalf("Result = %q", snap.Result)
	}
	if _, statErr := os.Stat(leaf); !os.IsNotExist(statErr) {
		t.Fatalf("the group outlived the kill that ended the start: %v", statErr)
	}
	if u.hasCgroupLeaf() {
		t.Fatal("the unit still believes it has a group")
	}
}

// The same wait must not turn into a hunt: a member that ignores the signal is
// still running, so the group stays and the process is left alone.
func TestATimeoutKeepsAGroupWhoseProcessIgnoresTheKill(t *testing.T) {
	stubCgroups(t)
	u := mkUnit("stubborn.service", "control-group")
	file := leafFile(t, u)
	// The shell cannot ignore a signal before it has said it will, so the test
	// waits for that promise rather than racing it.
	ready := filepath.Join(t.TempDir(), "ignoring")
	child := spawn(t, "/bin/sh", "-c", `trap "" TERM; echo ready > "$1"; while :; do sleep 1; done`, "sh", ready)
	waitForFile(t, ready)
	setMembers(t, file, child.Process.Pid)
	if err := syscall.Kill(child.Process.Pid, u.stopSignal()); err != nil {
		t.Fatal(err)
	}
	u.markTimeout(errors.New("notify timeout"), false)
	if !u.hasCgroupLeaf() {
		t.Fatalf("the group was dropped with %d still running in it", child.Process.Pid)
	}
	if !processAlive(child.Process.Pid) {
		t.Fatal("the release killed a process that refused the stop signal")
	}
}

func TestALiveMemberKeepsTheGroupAgainstTheRealTree(t *testing.T) {
	useRealTree(t)
	u := mkUnit("initd-livetest.service", "control-group")
	t.Cleanup(func() { _ = cgroupTree().Remove(u.cgroupID()) })
	if !u.prepareCgroup() {
		t.Fatal("no group could be created")
	}
	leaf, err := cgroupTree().LeafPath(u.cgroupID())
	if err != nil {
		t.Fatal(err)
	}
	child := spawn(t, "/bin/sleep", "60")
	if err := cgroupTree().Attach(u.cgroupID(), child.Process.Pid); err != nil {
		t.Fatalf("the kernel refused the move: %v", err)
	}
	// The main program is gone but this one is not: dropping the group now
	// would lose a process this unit started, and killing it is not this
	// code's business.
	u.transitionState(StateInactive, "")
	if !u.hasCgroupLeaf() {
		t.Fatal("the group was dropped while one of its processes was alive")
	}
	if _, statErr := os.Stat(leaf); statErr != nil {
		t.Fatalf("the group disappeared under a live member: %v", statErr)
	}
	if !processAlive(child.Process.Pid) {
		t.Fatal("releasing the group killed the process inside it")
	}
}

// The whole stop path, not just the sweep it ends with: the deferred cleanup
// runs after ExecStopPost and the runtime directories, and every call on the
// way takes the unit lock, so a group kill issued from under that lock would
// hang here rather than in production.
func TestStopOfARunningUnitSweepsItsGroup(t *testing.T) {
	stubCgroups(t)
	u := mkUnit("stopped.service", "control-group")
	file := leafFile(t, u)
	main := spawn(t, "/bin/sleep", "60")
	leftover := spawn(t, "/bin/sleep", "60")
	u.mu.Lock()
	u.Runtime.State = StateActive
	u.Runtime.MainPID = main.Process.Pid
	u.pgid = main.Process.Pid
	u.mu.Unlock()
	setMembers(t, file, main.Process.Pid, leftover.Process.Pid)

	if err := u.Stop(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	waitForDeath(t, main.Process.Pid, "the main process")
	waitForDeath(t, leftover.Process.Pid, "the grandchild that left the process group")
	if snap := u.Snapshot(); snap.State != StateInactive {
		t.Fatalf("state after the stop = %v", snap.State)
	}
	// Whether the leaf itself was released is not testable here: the kernel
	// rmdir's an empty cgroup directory, and a temporary directory that holds
	// the fixture's cgroup.procs is never empty. TestPlacementAndSweepAgainstTheRealTree
	// asserts that half against the real tree.
	if alive, known := u.cgroupAlive(); known && alive {
		t.Fatal("the group still reports a live member after the stop")
	}
}

// A leaf that lists nobody is not proof that the unit is idle: a process forked
// before the starter was moved in, or adopted from a pid file, belongs to the
// unit without ever being a member. The scan still gets the last word, which is
// what it was before cgroups existed here.
func TestAnEmptyGroupDoesNotVetoTheProcessGroupScan(t *testing.T) {
	stubCgroups(t)
	u := mkUnit("stray.service", "control-group")
	leafFile(t, u)
	stray := spawn(t, "/bin/sleep", "60")
	u.mu.Lock()
	u.pgid = stray.Process.Pid // spawned with Setpgid, so it leads this group
	u.mu.Unlock()
	if alive, known := u.cgroupAlive(); known && alive {
		t.Fatal("a group listing nobody claimed a live process")
	}
	if !u.unitGroupAlive(0, stray.Process.Pid, true) {
		t.Fatal("the unit reported idle with a live process in its recorded group")
	}
}

// emptyLeaf is a group the kernel would call childless: no cgroup.procs at all,
// so nothing claims a member and the leaf is ours to hand back.
func emptyLeaf(t *testing.T, u *Unit) string {
	t.Helper()
	if !u.prepareCgroup() {
		t.Fatal("prepareCgroup refused to create a group")
	}
	path, err := cgroupTree().LeafPath(u.cgroupID())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// A unit that ends by itself - a crash, an exit, a kill from outside - never
// runs a stop, so nothing else ever returned the group to the kernel. Leaked
// leaves are one per crash for the life of the supervisor, and a dead unit that
// still reports a ControlGroup is a lie either way.
func TestADeathNobodyAskedForReleasesTheGroup(t *testing.T) {
	stubCgroups(t)
	u := mkUnit("crashed.service", "control-group")
	leaf := emptyLeaf(t, u)
	u.transitionState(StateFailed, "exit status 1")
	if u.hasCgroupLeaf() {
		t.Fatal("the group was kept after the unit died with nobody in it")
	}
	if cg := u.ControlGroup(); cg != "" {
		t.Fatalf("ControlGroup() = %q for a unit that is gone", cg)
	}
	if _, err := os.Stat(leaf); !os.IsNotExist(err) {
		t.Fatalf("the leaf directory outlived the unit: %v", err)
	}
}

// A stop must not hand the group back on the way down: ExecStopPost still has
// to run inside it, and a helper that finds no group runs in the daemon's own,
// where it can see every process the supervisor has.
func TestAStopInProgressKeepsTheGroupForItsOwnEnding(t *testing.T) {
	stubCgroups(t)
	u := mkUnit("stopping.service", "control-group")
	emptyLeaf(t, u)
	u.mu.Lock()
	u.stopRequested = true
	u.mu.Unlock()
	u.transitionState(StateInactive, "")
	if !u.hasCgroupLeaf() {
		t.Fatal("the group was released while the stop still owed ExecStopPost a place to run")
	}
	u.finishCgroupStop()
	if u.hasCgroupLeaf() {
		t.Fatal("the stop's own ending did not release the group")
	}
}

// Something alive in the group is a process this unit started, whatever the
// main program did on the way out. Dropping the group then would lose it.
func TestALiveMemberKeepsTheGroupAfterTheMainProcessGoes(t *testing.T) {
	stubCgroups(t)
	u := mkUnit("orphan.service", "control-group")
	file := leafFile(t, u)
	child := spawn(t, "/bin/sleep", "60")
	setMembers(t, file, child.Process.Pid)
	u.transitionState(StateInactive, "")
	if !u.hasCgroupLeaf() {
		t.Fatalf("the group was dropped with %d still running in it", child.Process.Pid)
	}
	if !processAlive(child.Process.Pid) {
		t.Fatal("releasing a group killed the process inside it")
	}
}

// With no usable group every answer must be the one initd gave before this code
// existed: no group, no opinion, process groups used as they always were.
func TestEverythingStillWorksWithoutACgroupTree(t *testing.T) {
	unavailableCgroups(t)
	u := mkUnit("plain.service", "control-group")
	if u.prepareCgroup() {
		t.Fatal("prepareCgroup claimed a group on a box that has none")
	}
	if u.hasCgroupLeaf() {
		t.Fatal("the unit believes it has a group")
	}
	if cg := u.ControlGroup(); cg != "" {
		t.Fatalf("ControlGroup() = %q without a group", cg)
	}
	if pids := u.CGroupPIDs(); pids != nil {
		t.Fatalf("CGroupPIDs() = %v without a group, want no answer", pids)
	}
	if alive, known := u.cgroupAlive(); known || alive {
		t.Fatalf("cgroupAlive() = %v, %v without a group", alive, known)
	}
	child := spawn(t, "/bin/sleep", "60")
	u.mu.Lock()
	u.pgid = syscall.Getpgrp()
	u.mu.Unlock()
	u.placeInCgroup(child.Process.Pid)
	if pids := u.CGroupPIDs(); pids != nil {
		t.Fatalf("a start without a group recorded members: %v", pids)
	}
	// The fallback is the process group of the child, which it leads: the kill
	// has to reach it there, and must still leave the supervisor alone.
	u.signalUnitPID(child.Process.Pid, child.Process.Pid, true, syscall.SIGKILL)
	waitForDeath(t, child.Process.Pid, "the unit's process group")
	u.finishCgroupStop()
	if !processAlive(os.Getpid()) {
		t.Fatal("the fallback took the supervisor down")
	}
}
