package dbus

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	dbus "github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"

	"initd/internal/supervisor"
)

// systemd-style asynchronous jobs for the D-Bus Manager API.
//
// systemd's StartUnit & co. do not run the operation before replying; they
// enqueue a job and return its object path immediately. Clients that want to
// know whether the start actually worked subscribe to JobRemoved first and
// then filter for their job. initd used to run StartUnit synchronously and
// return the *unit* path, which no systemd client expects: a client waiting
// for JobRemoved waited forever, and a slow start blocked the caller on the
// job instead of the job tracking itself.
//
// The registry is per connection. Each job is an object at
// /org/freedesktop/systemd1/job/<id> exposing org.freedesktop.systemd1.Job
// properties for as long as it is queued or running; it is unexported when
// the job finishes, matching systemd, and ListJobs reports only
// not-yet-finished jobs. ClearJobs and CancelJob affect only jobs whose
// execution has not begun - the operation itself is an indivisible
// supervisor call and is never interrupted mid-flight.

const (
	jobPathPrefix = "/org/freedesktop/systemd1/job/"
	jobInterface  = "org.freedesktop.systemd1.Job"

	jobStateWaiting   = "waiting"
	jobStateRunning   = "running"
	jobResultDone     = "done"
	jobResultFailed   = "failed"
	jobResultCanceled = "canceled"
)

// listJobEntry matches systemd's a(usssoo) ListJobs return shape: the job id,
// the unit, the job type, the state, the job path, the unit path.
type listJobEntry struct {
	Id       uint32
	Unit     string
	JobType  string
	State    string
	Path     dbus.ObjectPath
	UnitPath dbus.ObjectPath
}

type job struct {
	id       uint32
	unit     string
	jobType  string
	state    string
	path     dbus.ObjectPath
	unitPath dbus.ObjectPath

	started  bool
	canceled bool
	props    *prop.Properties
}

// jobBus is the slice of the bus connection the registry needs, so tests can
// drive the lifecycle without a live bus.
type jobBus interface {
	exportProps(path dbus.ObjectPath, props prop.Map) (*prop.Properties, error)
	unexportProps(path dbus.ObjectPath)
	emit(signal string, values ...interface{}) error
}

type connJobBus struct{ conn *dbus.Conn }

func (b connJobBus) exportProps(path dbus.ObjectPath, props prop.Map) (*prop.Properties, error) {
	return prop.Export(b.conn, path, props)
}

func (b connJobBus) unexportProps(path dbus.ObjectPath) {
	// Export(nil, ...) unregisters the interface at path, and the path with
	// it once nothing else is exported there.
	_ = b.conn.Export(nil, path, propertiesInterface)
}

func (b connJobBus) emit(signal string, values ...interface{}) error {
	return b.conn.Emit(managerPath, signal, values...)
}

type jobRegistry struct {
	mu        sync.Mutex
	nextID    uint32
	jobs      map[uint32]*job
	installed uint32
	failed    uint32

	bus jobBus
	// onJobsChanged is called with (queued, installed, failed) after every
	// change; serveManager uses it to publish NJobs/NInstalledJobs/NFailedJobs.
	onJobsChanged func(nJobs, nInstalled, nFailed uint32)
	logf          func(string, ...interface{})
}

func newJobRegistry(bus jobBus, onJobsChanged func(nJobs, nInstalled, nFailed uint32), logf func(string, ...interface{})) *jobRegistry {
	return &jobRegistry{
		nextID:        1,
		jobs:          map[uint32]*job{},
		bus:           bus,
		onJobsChanged: onJobsChanged,
		logf:          logf,
	}
}

func (r *jobRegistry) log(format string, args ...interface{}) {
	if r.logf != nil {
		r.logf(format, args...)
	}
}

// newJob allocates a job, exports its properties object and emits JobNew.
// The caller starts the operation; the job is queued from this moment.
func (r *jobRegistry) newJob(unit, jobType string) *job {
	r.mu.Lock()
	id := r.nextID
	r.nextID++
	j := &job{
		id:       id,
		unit:     unit,
		jobType:  jobType,
		state:    jobStateWaiting,
		path:     dbus.ObjectPath(fmt.Sprintf("%s%d", jobPathPrefix, id)),
		unitPath: unitObjectPath(unit),
	}
	r.jobs[id] = j
	r.installed++
	r.mu.Unlock()

	if r.bus != nil {
		if props, err := r.bus.exportProps(j.path, jobProperties(j)); err == nil {
			j.props = props
		} else {
			r.log("job %d (%s %s): export properties: %v", id, jobType, unit, err)
		}
		if err := r.bus.emit("org.freedesktop.systemd1.Manager.JobNew", j.id, j.path, j.unit); err != nil {
			r.log("job %d (%s %s): emit JobNew: %v", id, jobType, unit, err)
		}
	}
	r.bumpJobs()
	return j
}

func jobProperties(j *job) prop.Map {
	return prop.Map{
		jobInterface: {
			"Id":      {Value: j.id, Writable: false, Emit: prop.EmitConst},
			"Unit":    {Value: j.unit, Writable: false, Emit: prop.EmitConst},
			"JobType": {Value: j.jobType, Writable: false, Emit: prop.EmitConst},
			"State":   {Value: j.state, Writable: false, Emit: prop.EmitTrue},
		},
	}
}

// begin marks the job as running. It reports false when the job was canceled
// while it was queued, in which case the operation must not run.
func (r *jobRegistry) begin(j *job) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if j.canceled {
		return false
	}
	j.started = true
	j.state = jobStateRunning
	return true
}

// finish removes the job from the queue, publishes its terminal state, emits
// JobRemoved and unexports the job object. result is one of done, failed,
// canceled.
func (r *jobRegistry) finish(j *job, result string) {
	r.mu.Lock()
	delete(r.jobs, j.id)
	if result == jobResultFailed {
		r.failed++
	}
	r.mu.Unlock()

	if j.props != nil {
		_ = j.props.Set(jobInterface, "State", dbus.MakeVariant(result))
	}
	if r.bus != nil {
		if err := r.bus.emit("org.freedesktop.systemd1.Manager.JobRemoved", j.id, j.path, j.unit, result); err != nil {
			r.log("job %d (%s %s): emit JobRemoved: %v", j.id, j.jobType, j.unit, err)
		}
		r.bus.unexportProps(j.path)
	}
	r.bumpJobs()
}

// cancel aborts a queued job. Jobs whose execution has already begun are left
// alone, as with systemd; cancellation is not injected into supervisor calls.
func (r *jobRegistry) cancel(id uint32) *dbus.Error {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return jobNoSuchJob(id)
	}
	if j.started || j.canceled {
		return nil
	}
	j.canceled = true
	j.state = jobResultCanceled
	return nil
}

// clearQueue cancels every job that has not begun executing. It reports how
// many were canceled.
func (r *jobRegistry) clearQueue() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, j := range r.jobs {
		if !j.started && !j.canceled {
			j.canceled = true
			j.state = jobResultCanceled
			n++
		}
	}
	return n
}

// list reports the queued and running jobs in id order.
func (r *jobRegistry) list() []listJobEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]listJobEntry, 0, len(r.jobs))
	for _, j := range r.jobs {
		out = append(out, listJobEntry{Id: j.id, Unit: j.unit, JobType: j.jobType, State: j.state, Path: j.path, UnitPath: j.unitPath})
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Id < out[k].Id })
	return out
}

// get returns the object path of a queued or running job.
func (r *jobRegistry) get(id uint32) (dbus.ObjectPath, *dbus.Error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return "", jobNoSuchJob(id)
	}
	return j.path, nil
}

func (r *jobRegistry) bumpJobs() {
	r.mu.Lock()
	nJobs := uint32(len(r.jobs))
	installed, failed := r.installed, r.failed
	r.mu.Unlock()
	if r.onJobsChanged != nil {
		r.onJobsChanged(nJobs, installed, failed)
	}
}

func jobNoSuchJob(id uint32) *dbus.Error {
	return &dbus.Error{Name: "org.freedesktop.systemd1.NoSuchJob", Body: []interface{}{fmt.Sprintf("Job %d not found.", id)}}
}

// jobUnitKnown reports whether the unit can be resolved before a job is
// queued, so a missing unit surfaces as NoSuchUnit instead of quietly
// failing inside a job.
func jobUnitKnown(mgr *supervisor.Manager, name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	if strings.HasSuffix(name, ".socket") {
		_, err := mgr.ShowSocketUnit(name)
		return err == nil
	}
	_, err := mgr.FindUnit(name)
	return err == nil
}

// enqueueUnitJob validates unit, enqueues the job, and runs run in the
// background. The job path is returned immediately; the outcome is reported
// through JobRemoved exactly once.
func enqueueUnitJob(mgr *supervisor.Manager, jobs *jobRegistry, logf func(string, ...interface{}), unit, jobType string, run func(string) error) (dbus.ObjectPath, *dbus.Error) {
	if !jobUnitKnown(mgr, unit) {
		return "", &dbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit", Body: []interface{}{fmt.Sprintf("Unit %s not found.", unit)}}
	}
	job := jobs.newJob(unit, jobType)
	go func() {
		if !jobs.begin(job) {
			// Canceled while queued.
			jobs.finish(job, jobResultCanceled)
			return
		}
		if err := run(unit); err != nil {
			if logf != nil {
				logf("job %d (%s %s) failed: %v", job.id, jobType, unit, err)
			}
			jobs.finish(job, jobResultFailed)
			return
		}
		jobs.finish(job, jobResultDone)
	}()
	return job.path, nil
}
