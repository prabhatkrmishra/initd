package supervisor

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// stubScopeCgroups points the cgroup machinery at a temporary directory so
// tests never create leaves in the host's real cgroup tree.
func stubScopeCgroups(t *testing.T) {
	t.Helper()
	t.Setenv("INITD_CGROUP_ROOT", t.TempDir()+"/initd")
}

func spawnScopeSleep(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "60")
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

func TestStartTransientScopeActivatesAndSurvivesLoadUnits(t *testing.T) {
	stubScopeCgroups(t)
	m, _ := newTestManager(t)
	cmd := spawnScopeSleep(t)
	pid := cmd.Process.Pid

	if err := m.StartTransientScope("zztransit.scope", TransientScope{
		Description: "test transient scope",
		PIDs:        []int{pid},
	}); err != nil {
		t.Fatalf("StartTransientScope: %v", err)
	}
	unit, err := m.FindUnit("zztransit.scope")
	if err != nil {
		t.Fatalf("FindUnit after creation: %v", err)
	}
	if got := unit.Snapshot().State; got != "active" {
		t.Fatalf("state = %q, want active", got)
	}
	// Reload must not orphan an active scope.
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	unit, err = m.FindUnit("zztransit.scope")
	if err != nil {
		t.Fatal("transient scope was lost after LoadUnits")
	}
	if got := unit.Snapshot().State; got != "active" {
		t.Fatalf("state after LoadUnits = %q, want active", got)
	}
	// Stop kills the member and unloads the scope.
	if err := unit.Stop(5 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		m.mu.Lock()
		_, exists := m.Units["zztransit.scope"]
		m.mu.Unlock()
		if !exists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stopped scope was not unloaded")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStartTransientScopeDuplicateRefused(t *testing.T) {
	stubScopeCgroups(t)
	m, _ := newTestManager(t)
	cmd := spawnScopeSleep(t)

	if err := m.StartTransientScope("zzdup.scope", TransientScope{PIDs: []int{cmd.Process.Pid}}); err != nil {
		t.Fatalf("first StartTransientScope: %v", err)
	}
	err := m.StartTransientScope("zzdup.scope", TransientScope{PIDs: []int{cmd.Process.Pid}})
	if err == nil {
		t.Fatal("second creation with the same name must fail")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v, want 'already exists'", err)
	}
	unit, _ := m.FindUnit("zzdup.scope")
	_ = unit.Stop(5 * time.Second)
}

func TestStartTransientScopeNonScopeNameRefused(t *testing.T) {
	stubScopeCgroups(t)
	m, _ := newTestManager(t)
	if err := m.StartTransientScope("zzwrong.service", TransientScope{PIDs: []int{os.Getpid()}}); err == nil {
		t.Fatal("non-.scope transient creation must fail")
	}
}

func TestTransientScopeUnloadFiresHandler(t *testing.T) {
	stubScopeCgroups(t)
	m, _ := newTestManager(t)
	removed := make(chan string, 1)
	m.AddUnitRemovedHandler(func(name string) { removed <- name })

	cmd := exec.Command("/bin/sleep", "0.15")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start /bin/sleep: %v", err)
	}
	if err := m.StartTransientScope("zzunload.scope", TransientScope{PIDs: []int{cmd.Process.Pid}}); err != nil {
		t.Fatalf("StartTransientScope: %v", err)
	}
	_ = cmd.Wait()

	select {
	case name := <-removed:
		if name != "zzunload.scope" {
			t.Fatalf("handler saw %q, want zzunload.scope", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the unloaded scope fired no handler")
	}
}

func TestStartTransientScopeGoesEmptyAndUnloads(t *testing.T) {
	stubScopeCgroups(t)
	m, _ := newTestManager(t)
	cmd := exec.Command("/bin/sleep", "0.15")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start /bin/sleep: %v", err)
	}
	if err := m.StartTransientScope("zzselfempty.scope", TransientScope{PIDs: []int{cmd.Process.Pid}}); err != nil {
		t.Fatalf("StartTransientScope: %v", err)
	}
	_ = cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		unit, exists := m.Units["zzselfempty.scope"]
		m.mu.Unlock()
		if !exists {
			return
		}
		if snap := unit.Snapshot(); snap.State == "inactive" {
			// The unit is inactive; it should unload itself shortly.
			time.Sleep(300 * time.Millisecond)
			m.mu.Lock()
			_, stillThere := m.Units["zzselfempty.scope"]
			m.mu.Unlock()
			if !stillThere {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the emptied scope did not unload")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
