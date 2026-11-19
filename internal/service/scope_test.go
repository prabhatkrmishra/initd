package service

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"initd/internal/parser"
)

func mkScopeUnit(t *testing.T, name string, remainAfterExit bool) *Unit {
	t.Helper()
	cfg := &parser.Unit{Name: name}
	cfg.Description = "test scope"
	cfg.Service.Type = "scope"
	if remainAfterExit {
		cfg.Service.RemainAfterExit = "yes"
	}
	return NewUnit(cfg, name)
}

func spawnSleep(t *testing.T, d time.Duration) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sleep", d.String())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start /bin/sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

func TestScopeActivatesAroundPIDs(t *testing.T) {
	unavailableCgroups(t)
	u := mkScopeUnit(t, "zzscope-activate.scope", false)
	p1 := spawnSleep(t, time.Minute)
	p2 := spawnSleep(t, time.Minute)

	if !u.isScope() {
		t.Fatal("isScope() = false for a Type=scope unit")
	}
	if err := u.ActivateScope([]int{p1.Process.Pid, p2.Process.Pid}, false); err != nil {
		t.Fatalf("ActivateScope: %v", err)
	}
	snap := u.Snapshot()
	if snap.State != StateActive {
		t.Fatalf("state = %q, want active", snap.State)
	}
	if snap.MainPID != 0 {
		t.Fatalf("MainPID = %d, want 0 (scopes have no main process)", snap.MainPID)
	}
	// Starting an already-active scope is refused.
	if err := u.ActivateScope([]int{p1.Process.Pid}, false); err == nil {
		t.Fatal("re-activation of an active scope must fail")
	}
	_ = u.Stop(2 * time.Second)
}

func TestScopeStopKillsMemberPIDs(t *testing.T) {
	stubCgroups(t)
	u := mkScopeUnit(t, "zzscope-stop.scope", false)
	leafFile(t, u)

	p1 := spawnSleep(t, time.Minute)
	p2 := spawnSleep(t, time.Minute)
	pid1, pid2 := p1.Process.Pid, p2.Process.Pid
	if err := u.ActivateScope([]int{pid1, pid2}, false); err != nil {
		t.Fatalf("ActivateScope: %v", err)
	}
	if members := u.CGroupPIDs(); len(members) != 2 {
		t.Fatalf("CGroupPIDs = %v, want the two PIDs", members)
	}
	if cg := u.ControlGroup(); !strings.HasSuffix(cg, "/zzscope-stop.scope") {
		t.Fatalf("ControlGroup = %q", cg)
	}

	if err := u.Stop(5 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitForDeath(t, pid1, "the first scope member")
	waitForDeath(t, pid2, "the second scope member")
	if got := u.Snapshot().State; got != StateInactive {
		t.Fatalf("state after stop = %q, want inactive", got)
	}
}

// Only the kernel can give the group back once it is emptied; run with
// INITD_CGROUP_REAL=1 on a box with a writable cgroup prefix.
func TestScopeStopReleasesLeafAgainstTheRealTree(t *testing.T) {
	useRealTree(t)
	u := mkScopeUnit(t, "zzscope-release.scope", false)
	t.Cleanup(func() { _ = cgroupTree().Remove(u.cgroupID()) })
	if !u.prepareCgroup() {
		t.Fatal("no group could be created")
	}
	p1 := spawnSleep(t, time.Minute)
	if err := u.ActivateScope([]int{p1.Process.Pid}, false); err != nil {
		t.Fatalf("ActivateScope: %v", err)
	}
	if err := u.Stop(5 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitForDeath(t, p1.Process.Pid, "the scope member")
	if u.hasCgroupLeaf() {
		t.Fatal("the group survived the stop that emptied it")
	}
}

func TestScopeDeactivatesWhenPIDsExitOnTheirOwn(t *testing.T) {
	// No cgroup: liveness comes from the tracked PIDs.
	unavailableCgroups(t)
	u := mkScopeUnit(t, "zzscope-exit.scope", false)
	p := spawnSleep(t, 200*time.Millisecond)
	if err := u.ActivateScope([]int{p.Process.Pid}, false); err != nil {
		t.Fatalf("ActivateScope: %v", err)
	}
	waitForState(t, u, StateInactive)
	snap := u.Snapshot()
	if snap.Result != "success" {
		t.Fatalf("Result = %q, want success", snap.Result)
	}
}

func TestScopeDeactivatesWhenPIDsExitWithCgroup(t *testing.T) {
	stubCgroups(t)
	u := mkScopeUnit(t, "zzscope-exitcg.scope", false)
	leafFile(t, u)
	p := spawnSleep(t, 200*time.Millisecond)
	if err := u.ActivateScope([]int{p.Process.Pid}, false); err != nil {
		t.Fatalf("ActivateScope: %v", err)
	}
	waitForState(t, u, StateInactive)
}

func TestScopeRemainAfterExitStaysActive(t *testing.T) {
	unavailableCgroups(t)
	u := mkScopeUnit(t, "zzscope-remain.scope", true)
	p := spawnSleep(t, 100*time.Millisecond)
	if err := u.ActivateScope([]int{p.Process.Pid}, true); err != nil {
		t.Fatalf("ActivateScope: %v", err)
	}
	// The PID is dead, but RemainAfterExit keeps the scope active.
	waitForDeath(t, p.Process.Pid, "the scope member")
	time.Sleep(300 * time.Millisecond)
	if got := u.Snapshot().State; got != StateActive {
		t.Fatalf("state = %q, want active (RemainAfterExit)", got)
	}
	_ = u.Stop(2 * time.Second)
}

func TestScopeEmptinessCallbackFires(t *testing.T) {
	unavailableCgroups(t)
	u := mkScopeUnit(t, "zzscope-cb.scope", false)
	emptied := make(chan string, 1)
	u.SetScopeEmptyHandler(func(name string) { emptied <- name })
	p := spawnSleep(t, 100*time.Millisecond)
	if err := u.ActivateScope([]int{p.Process.Pid}, false); err != nil {
		t.Fatalf("ActivateScope: %v", err)
	}
	select {
	case name := <-emptied:
		if name != "zzscope-cb.scope" {
			t.Fatalf("callback fired with %q", name)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("scope emptied callback never fired")
	}
}
