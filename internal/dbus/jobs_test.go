package dbus

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	dbus "github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
)

// fakeJobBus records what the registry puts on the bus.
type fakeJobBus struct {
	mu         sync.Mutex
	signals    []emittedSignal
	exported   []dbus.ObjectPath
	unexported []dbus.ObjectPath
}

type emittedSignal struct {
	signal string
	values []interface{}
}

func (b *fakeJobBus) exportProps(path dbus.ObjectPath, props prop.Map) (*prop.Properties, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.exported = append(b.exported, path)
	return nil, nil
}

func (b *fakeJobBus) unexportProps(path dbus.ObjectPath) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.unexported = append(b.unexported, path)
}

func (b *fakeJobBus) emit(signal string, values ...interface{}) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.signals = append(b.signals, emittedSignal{signal: signal, values: values})
	return nil
}

func (b *fakeJobBus) signalsOf(member string) []emittedSignal {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []emittedSignal
	for _, s := range b.signals {
		if strings.HasSuffix(s.signal, "."+member) {
			out = append(out, s)
		}
	}
	return out
}

// jobTestHarness collects the onJobsChanged notifications.
type jobTestHarness struct {
	bus           *fakeJobBus
	registry      *jobRegistry
	mu            sync.Mutex
	notifications [][3]uint32
}

func newJobTestHarness() *jobTestHarness {
	h := &jobTestHarness{bus: &fakeJobBus{}}
	h.registry = newJobRegistry(h.bus, func(nJobs, nInstalled, nFailed uint32) {
		h.mu.Lock()
		h.notifications = append(h.notifications, [3]uint32{nJobs, nInstalled, nFailed})
		h.mu.Unlock()
	}, nil)
	return h
}

func (h *jobTestHarness) lastNotification() (bool, [3]uint32) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.notifications) == 0 {
		return false, [3]uint32{}
	}
	return true, h.notifications[len(h.notifications)-1]
}

func TestJobNewJobGetAndPath(t *testing.T) {
	h := newJobTestHarness()
	j := h.registry.newJob("foo.service", "start")
	if j.id != 1 {
		t.Fatalf("first job id = %d, want 1", j.id)
	}
	if string(j.path) != "/org/freedesktop/systemd1/job/1" {
		t.Fatalf("path = %q", j.path)
	}
	j2 := h.registry.newJob("bar.service", "stop")
	if j2.id != 2 {
		t.Fatalf("second job id = %d, want 2", j2.id)
	}
	path, derr := h.registry.get(j.id)
	if derr != nil {
		t.Fatalf("get: %v", derr)
	}
	if path != j.path {
		t.Fatalf("get path = %q, want %q", path, j.path)
	}
	if _, derr := h.registry.get(99); derr == nil || derr.Name != "org.freedesktop.systemd1.NoSuchJob" {
		t.Fatalf("get(99) = %v, want NoSuchJob", derr)
	}
}

func TestJobLifecycleEmitsJobNewAndJobRemoved(t *testing.T) {
	h := newJobTestHarness()
	j := h.registry.newJob("foo.service", "start")

	news := h.bus.signalsOf("JobNew")
	if len(news) != 1 {
		t.Fatalf("JobNew signals = %d, want 1", len(news))
	}
	got := news[0]
	if got.values[0].(uint32) != 1 || got.values[1].(dbus.ObjectPath) != j.path || got.values[2].(string) != "foo.service" {
		t.Fatalf("JobNew args = %v", got.values)
	}
	if ok, n := h.lastNotification(); !ok || n != [3]uint32{1, 1, 0} {
		t.Fatalf("after newJob notification = %v, %v", ok, n)
	}

	if !h.registry.begin(j) {
		t.Fatal("begin = false for a fresh job")
	}
	h.registry.finish(j, jobResultDone)

	removed := h.bus.signalsOf("JobRemoved")
	if len(removed) != 1 {
		t.Fatalf("JobRemoved signals = %d, want 1", len(removed))
	}
	got = removed[0]
	if got.values[0].(uint32) != j.id || got.values[1].(dbus.ObjectPath) != j.path || got.values[2].(string) != "foo.service" || got.values[3].(string) != "done" {
		t.Fatalf("JobRemoved args = %v", got.values)
	}
	// The finished job is gone from the queue and its object is unexported.
	if jobs := h.registry.list(); len(jobs) != 0 {
		t.Fatalf("jobs after finish = %v, want none", jobs)
	}
	if _, derr := h.registry.get(j.id); derr == nil || derr.Name != "org.freedesktop.systemd1.NoSuchJob" {
		t.Fatalf("get(finished) = %v, want NoSuchJob", derr)
	}
	if len(h.bus.unexported) != 1 || h.bus.unexported[0] != j.path {
		t.Fatalf("unexported = %v, want [%s]", h.bus.unexported, j.path)
	}
	if ok, n := h.lastNotification(); !ok || n != [3]uint32{0, 1, 0} {
		t.Fatalf("after finish notification = %v, %v", ok, n)
	}
}

func TestJobFailureCountsAsFailed(t *testing.T) {
	h := newJobTestHarness()
	j := h.registry.newJob("boom.service", "start")
	h.registry.begin(j)
	h.registry.finish(j, jobResultFailed)

	if ok, n := h.lastNotification(); !ok || n != [3]uint32{0, 1, 1} {
		t.Fatalf("notification = %v, %v, want [0 1 1]", ok, n)
	}
	removed := h.bus.signalsOf("JobRemoved")
	if len(removed) != 1 || removed[0].values[3].(string) != "failed" {
		t.Fatalf("JobRemoved = %v, want result=failed", removed)
	}
}

func TestCancelWaitingJobPreventsExecution(t *testing.T) {
	h := newJobTestHarness()
	j := h.registry.newJob("slow.service", "start")

	if err := h.registry.cancel(j.id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if h.registry.begin(j) {
		t.Fatal("begin = true for a canceled job")
	}
	h.registry.finish(j, jobResultCanceled)
	if jobs := h.registry.list(); len(jobs) != 0 {
		t.Fatalf("jobs = %v, want none", jobs)
	}
	removed := h.bus.signalsOf("JobRemoved")
	if len(removed) != 1 || removed[0].values[3].(string) != "canceled" {
		t.Fatalf("JobRemoved = %v, want result=canceled", removed)
	}
}

func TestCancelRunningJobIsANoop(t *testing.T) {
	h := newJobTestHarness()
	j := h.registry.newJob("run.service", "start")
	if !h.registry.begin(j) {
		t.Fatal("begin = false")
	}
	if err := h.registry.cancel(j.id); err != nil {
		t.Fatalf("cancel of running job: %v", err)
	}
	// The job still reports as running and can finish normally.
	jobs := h.registry.list()
	if len(jobs) != 1 || jobs[0].State != jobStateRunning {
		t.Fatalf("list = %v, want one running job", jobs)
	}
	h.registry.finish(j, jobResultDone)
}

func TestCancelUnknownJob(t *testing.T) {
	h := newJobTestHarness()
	err := h.registry.cancel(42)
	if err == nil || err.Name != "org.freedesktop.systemd1.NoSuchJob" {
		t.Fatalf("cancel(42) = %v, want NoSuchJob", err)
	}
}

func TestClearJobsCancelsOnlyQueuedJobs(t *testing.T) {
	h := newJobTestHarness()
	waiting1 := h.registry.newJob("a.service", "start")
	running := h.registry.newJob("b.service", "start")
	waiting2 := h.registry.newJob("c.service", "start")

	if !h.registry.begin(running) {
		t.Fatal("begin = false")
	}
	if n := h.registry.clearQueue(); n != 2 {
		t.Fatalf("clearQueue canceled %d, want 2", n)
	}
	if h.registry.begin(waiting1) || h.registry.begin(waiting2) {
		t.Fatal("a cleared job must not run")
	}
	if !h.registry.begin(running) {
		// begin was already true; state must remain running by not being canceled.
		t.Fatal("running job must not be canceled by ClearJobs")
	}
	// The running job is still queued (not canceled).
	jobs := h.registry.list()
	if len(jobs) != 3 {
		t.Fatalf("list = %d jobs, want 3 (cleared jobs stay listed until finished)", len(jobs))
	}
	h.registry.finish(waiting1, jobResultCanceled)
	h.registry.finish(waiting2, jobResultCanceled)
	h.registry.finish(running, jobResultDone)
	if jobs := h.registry.list(); len(jobs) != 0 {
		t.Fatalf("list after finish = %d jobs, want 0", len(jobs))
	}
}

func TestListJobsSortedByID(t *testing.T) {
	h := newJobTestHarness()
	for i := 0; i < 10; i++ {
		h.registry.newJob(fmt.Sprintf("u%d.service", i), "start")
	}
	jobs := h.registry.list()
	for i, j := range jobs {
		if j.Id != uint32(i+1) {
			t.Fatalf("jobs[%d].Id = %d, want %d", i, j.Id, i+1)
		}
	}
}

func waitForJobs(t *testing.T, r *jobRegistry, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(r.list()) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("jobs never became %d (now %d)", want, len(r.list()))
}

func TestEnqueueUnitJobReturnsBeforeOperationCompletes(t *testing.T) {
	mgr := newTestManager(t)
	dir := mgr.SearchPaths[0]
	writeUnitForDBus(t, dir, "zzblockjob.service", "[Service]\nExecStart=/bin/sleep 30\n")
	if err := mgr.LoadUnits(); err != nil {
		t.Fatal(err)
	}
	jobs := newJobRegistry(nil, nil, nil)

	release := make(chan string, 1)
	start := time.Now()
	path, derr := enqueueUnitJob(mgr, jobs, nil, "zzblockjob.service", "start", func(unit string) error {
		release <- unit
		return nil
	})
	if derr != nil {
		t.Fatalf("enqueue: %v", derr)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("enqueue blocked on the operation")
	}
	unit := <-release
	if unit != "zzblockjob.service" {
		t.Fatalf("run called with %q", unit)
	}
	waitForJobs(t, jobs, 0)
	_ = path
	_ = mgr.StopUnit("zzblockjob.service")
}

func TestEnqueueUnitJobFailureResult(t *testing.T) {
	mgr := newTestManager(t)
	dir := mgr.SearchPaths[0]
	writeUnitForDBus(t, dir, "zzfailjob.service", "[Service]\nExecStart=/bin/true\n")
	if err := mgr.LoadUnits(); err != nil {
		t.Fatal(err)
	}
	var lastNotif [3]uint32
	var mu sync.Mutex
	jobs := newJobRegistry(nil, func(nJobs, nInstalled, nFailed uint32) {
		mu.Lock()
		lastNotif = [3]uint32{nJobs, nInstalled, nFailed}
		mu.Unlock()
	}, nil)

	if _, derr := enqueueUnitJob(mgr, jobs, nil, "zzfailjob.service", "start", func(string) error {
		return fmt.Errorf("kaboom")
	}); derr != nil {
		t.Fatalf("enqueue: %v", derr)
	}
	waitForJobs(t, jobs, 0)
	mu.Lock()
	defer mu.Unlock()
	if lastNotif != [3]uint32{0, 1, 1} {
		t.Fatalf("last notification = %v, want [0 1 1]", lastNotif)
	}
}

func TestStartUnitMissingUnitNotEnqueued(t *testing.T) {
	mgr := newTestManager(t)
	m := newManager(mgr)
	if _, derr := m.StartUnit("no-such.service", "fail"); derr == nil || derr.Name != "org.freedesktop.systemd1.NoSuchUnit" {
		t.Fatalf("StartUnit(missing) = %v, want NoSuchUnit", derr)
	}
	if jobs := m.jobs.list(); len(jobs) != 0 {
		t.Fatalf("jobs = %v, want none after refusal", jobs)
	}
	entries, derr := m.ListJobs()
	if derr != nil {
		t.Fatalf("ListJobs: %v", derr)
	}
	if len(entries) != 0 {
		t.Fatalf("ListJobs = %v, want empty", entries)
	}
}

func TestEnqueueUnitJobMissingUnitIsSynchronousNoSuchUnit(t *testing.T) {
	mgr := newTestManager(t)
	jmgr := newJobRegistry(nil, nil, nil)
	called := false
	path, derr := enqueueUnitJob(mgr, jmgr, nil, "no-such-unit.service", "start", func(string) error {
		called = true
		return nil
	})
	if derr == nil {
		t.Fatal("missing unit must be refused synchronously")
	}
	if derr.Name != "org.freedesktop.systemd1.NoSuchUnit" {
		t.Fatalf("error name = %q, want NoSuchUnit", derr.Name)
	}
	if path != "" {
		t.Fatalf("path = %q, want empty", path)
	}
	if called {
		t.Fatal("the operation must not run for a missing unit")
	}
	if jobs := jmgr.list(); len(jobs) != 0 {
		t.Fatalf("jobs = %v, want none", jobs)
	}
}

func TestStartUnitReturnsJobPath(t *testing.T) {
	mgr := newTestManager(t)
	dir := mgr.SearchPaths[0]
	writeUnitForDBus(t, dir, "zzjobmethod.service", "[Service]\nExecStart=/bin/sleep 30\n")
	if err := mgr.LoadUnits(); err != nil {
		t.Fatal(err)
	}
	m := newManager(mgr)

	path, derr := m.StartUnit("zzjobmethod.service", "fail")
	if derr != nil {
		t.Fatalf("StartUnit: %v", derr)
	}
	if !strings.HasPrefix(string(path), "/org/freedesktop/systemd1/job/") {
		t.Fatalf("path = %q, want a job path", path)
	}
	// The job is tracked while it runs.
	deadline := time.Now().Add(5 * time.Second)
	for {
		jobs := m.jobs.list()
		if len(jobs) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, derr := m.GetJob(1); derr == nil || derr.Name != "org.freedesktop.systemd1.NoSuchJob" {
		t.Fatalf("GetJob(1) after finish = %v, want NoSuchJob", derr)
	}
	if derr := m.CancelJob(999); derr == nil || derr.Name != "org.freedesktop.systemd1.NoSuchJob" {
		t.Fatalf("CancelJob(999) = %v, want NoSuchJob", derr)
	}
	_ = mgr.StopUnit("zzjobmethod.service")
}

func TestStartUnitMethodWithMissingUnitLeavesNoJobs(t *testing.T) {
	mgr := newTestManager(t)
	m := newManager(mgr)
	if _, derr := m.StartUnit("no-such.service", "fail"); derr == nil || derr.Name != "org.freedesktop.systemd1.NoSuchUnit" {
		t.Fatalf("StartUnit(missing) = %v, want NoSuchUnit", derr)
	}
	if jobs := m.jobs.list(); len(jobs) != 0 {
		t.Fatalf("jobs = %v, want none after refusal", jobs)
	}
	entries, derr := m.ListJobs()
	if derr != nil {
		t.Fatalf("ListJobs: %v", derr)
	}
	if len(entries) != 0 {
		t.Fatalf("ListJobs = %v, want empty", entries)
	}
}
