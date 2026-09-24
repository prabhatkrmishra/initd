package supervisor

import (
	"sync"
	"testing"
	"time"

	"initd/internal/service"
)

// Required dependencies must fail the dependent when they never become
// ready, using their own start timeout instead of a fixed 30s pass.
func TestRequiredDependencyTimeoutFails(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "dep.service", "[Service]\nType=notify\nExecStart=/bin/sleep 30\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatal(err)
	}
	dep, err := m.FindUnit("dep.service")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dep.Start(); err != nil {
		t.Fatalf("start dep: %v", err)
	}
	// Wait until it sits in activating (notify never READY).
	deadline := time.Now().Add(5 * time.Second)
	for {
		if snap := dep.Snapshot(); snap.State == service.StateActivating {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dep never entered activating")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := m.waitForUnitReady(dep, 200*time.Millisecond); err == nil {
		t.Fatalf("waitForUnitReady on stuck activating unit returned nil; want timeout error")
	}
	_ = dep.Stop(dep.StopTimeout())
}

// PartOf chains must propagate transitively: stopping C stops B and A.
func TestStopPartOfRecursive(t *testing.T) {
	m, dir := newTestManager(t)
	writeUnit(t, dir, "a.service", "[Unit]\nPartOf=b.service\n[Service]\nExecStart=/bin/sleep 30\n")
	writeUnit(t, dir, "b.service", "[Unit]\nPartOf=c.service\n[Service]\nExecStart=/bin/sleep 30\n")
	writeUnit(t, dir, "c.service", "[Service]\nExecStart=/bin/sleep 30\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.service", "b.service", "c.service"} {
		if err := m.StartUnit(n); err != nil {
			t.Fatalf("start %s: %v", n, err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if err := m.StopUnit("c.service"); err != nil {
		t.Fatalf("stop c: %v", err)
	}
	for _, n := range []string{"a.service", "b.service", "c.service"} {
		u, _ := m.FindUnit(n)
		if snap := u.Snapshot(); snap.State != service.StateInactive {
			t.Fatalf("%s = %v, want inactive after recursive stop", n, snap.State)
		}
	}
}

// Concurrent socket starts must not create duplicate listeners.
func TestSocketDoubleStart(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := dir + "/dbl.sock"
	writeUnit(t, dir, "dbl.socket", "[Socket]\nListenStream="+sockPath+"\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = m.StartUnit("dbl.socket")
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent start: %v", err)
		}
	}
	m.mu.Lock()
	rt := m.SocketRuntimes["dbl.socket"]
	n := len(rt.listeners)
	m.mu.Unlock()
	if n != 1 {
		t.Fatalf("listeners = %d, want 1 after concurrent starts", n)
	}
	_ = m.StopUnit("dbl.socket")
}
