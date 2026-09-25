package service

import (
	"errors"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"initd/internal/cgroup"
	"initd/internal/logging"
)

// cgroupTree is the daemon's view of the kernel's cgroup layout. Tests replace
// it with a tree over a temporary directory.
var cgroupTree = cgroup.Default

// cgroupID is the leaf this unit's processes are grouped under.
func (u *Unit) cgroupID() string { return u.GetConfig().Name }

func (u *Unit) hasCgroupLeaf() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.cgroupLeaf
}

// prepareCgroup creates the unit's leaf before anything runs in it. A box with
// no usable tree answers false forever, and every caller then behaves exactly
// as it did before cgroups existed here.
func (u *Unit) prepareCgroup() bool {
	u.mu.Lock()
	if u.cgroupLeaf {
		u.mu.Unlock()
		return true
	}
	u.mu.Unlock()
	tree := cgroupTree()
	if !tree.Available() {
		return false
	}
	if err := tree.Ensure(u.cgroupID()); err != nil {
		u.Log(logging.LevelInfo, "Warning: unit cgroup unavailable: "+err.Error())
		return false
	}
	u.mu.Lock()
	u.cgroupLeaf = true
	u.mu.Unlock()
	return true
}

// placeInCgroup makes the kernel attribute one process to this unit. Go cannot
// fork straight into a cgroup, so the child is born in the daemon's group and
// moved afterwards: anything it forked before the move stays outside the leaf,
// which is why signalUnitPID sweeps the process group as well.
func (u *Unit) placeInCgroup(pid int) {
	if pid <= 0 || !u.hasCgroupLeaf() {
		return
	}
	if err := cgroupTree().Attach(u.cgroupID(), pid); err != nil {
		u.Log(logging.LevelInfo, "Warning: could not place process in unit cgroup: "+err.Error())
	}
}

// cgroupMembers is the kernel's answer about who this unit runs, or nil when
// there is no group to ask. Callers must read nil as "no evidence" and fall
// back, never as "nobody".
func (u *Unit) cgroupMembers() []int {
	if !u.hasCgroupLeaf() {
		return nil
	}
	pids, err := cgroupTree().Members(u.cgroupID())
	if err != nil {
		return nil
	}
	return pids
}

// cgroupAlive reports what the group says about liveness, and whether the group
// had anything to say.
func (u *Unit) cgroupAlive() (alive bool, known bool) {
	members := u.cgroupMembers()
	if members == nil {
		return false, false
	}
	for _, pid := range members {
		if pid != os.Getpid() && checkLiveness(pid) != livenessDead {
			return true, true
		}
	}
	return false, true
}

// cgroupSweep signals every process in the unit's group. The daemon is never a
// member, so the sweep cannot take the supervisor down the way a -pgid signal
// could when groups collapsed; it is filtered anyway because signalling the
// caller of a stop is never right.
func (u *Unit) cgroupSweep(sig syscall.Signal) int {
	members := u.cgroupMembers()
	if members == nil {
		return 0
	}
	sent := 0
	for _, pid := range members {
		if pid <= 0 || pid == os.Getpid() {
			continue
		}
		if err := syscall.Kill(pid, sig); err == nil || err == syscall.ESRCH {
			sent++
		}
	}
	return sent
}

// cgroupMemberPID picks the process a forking or notifying daemon left behind,
// from the unit's group. A group with one member is unambiguous; several means
// something outlived the start alongside the daemon, and the newest is the one
// this start made. Without a group there is nothing to say, which callers
// answer the way they did before cgroups.
func (u *Unit) cgroupMemberPID(exclude int) int {
	members := u.cgroupMembers()
	if members == nil {
		return 0
	}
	u.mu.Lock()
	since := u.Runtime.StartedAt
	u.mu.Unlock()
	type candidate struct {
		pid   int
		start time.Time
	}
	var live []candidate
	for _, pid := range members {
		if pid == exclude || pid == os.Getpid() || checkLiveness(pid) == livenessDead {
			continue
		}
		start, ok := procStartTime(pid)
		if !ok {
			// Undated: only chosen when nothing else is left.
			start = time.Time{}
		} else if !since.IsZero() && start.Before(since.Add(-2*time.Second)) {
			// A process older than this start cannot be what it created, and a
			// restarted unit's group can still hold a survivor of the last one.
			continue
		}
		live = append(live, candidate{pid, start})
	}
	if len(live) == 0 {
		return 0
	}
	sort.SliceStable(live, func(i, j int) bool { return live[i].start.Before(live[j].start) })
	// Members of one group are the unit's own processes; the argument that
	// guards a recycled process group (a stranger may hold the number) does not
	// apply, so no ownership test here.
	return live[len(live)-1].pid
}

// killModeSweepsLeftovers reports whether a stop is expected to account for
// every process the unit started: control-group signals them all, mixed lets
// the main process go first and then kills the rest, and process and none
// deliberately leave children running.
func (u *Unit) killModeSweepsLeftovers() bool {
	switch strings.ToLower(strings.TrimSpace(u.GetConfig().Service.KillMode)) {
	case "control-group", "controlgroup", "control_group", "mixed":
		return true
	}
	return false
}

// cgroupSettle is how long the daemon waits for a signal it has already sent to
// land: the kernel does not act on it until the target runs again, so a group
// can hold a process that is on its way out.
const cgroupSettle = 500 * time.Millisecond

// cgroupPoll is how often a settle loop asks the group what it holds.
const cgroupPoll = 20 * time.Millisecond

// finishCgroupStop is the second half of KillMode=mixed and the cleanup any
// stop owes the kernel: everything left in the group is killed, and the group
// is dropped. A process that cannot be killed keeps the leaf alive (the kernel
// refuses to remove a group that still has members) and stays attributed to
// this unit, which is the honest outcome: it is ours, and it is still running.
func (u *Unit) finishCgroupStop() {
	if !u.hasCgroupLeaf() {
		return
	}
	if u.killModeSweepsLeftovers() {
		deadline := time.Now().Add(cgroupSettle)
		for {
			alive, known := u.cgroupAlive()
			if !known || !alive || time.Now().After(deadline) {
				break
			}
			u.cgroupSweep(syscall.SIGKILL)
			time.Sleep(cgroupPoll)
		}
	}
	u.dropEmptyCgroup()
}

// releaseEmptyCgroup is the release a unit that died on its own still owes: no
// stop is coming, so nothing else would hand the group back, and the daemon
// would go on reporting a ControlGroup that holds nobody. It is skipped while a
// stop is in progress, because that stop still owes ExecStopPost a group to run
// inside - a helper with no group of its own ends up in the daemon's, where it
// can see every process the supervisor has.
func (u *Unit) releaseEmptyCgroup() {
	u.mu.Lock()
	inStop := u.stopRequested
	u.mu.Unlock()
	if inStop {
		return
	}
	// A start that timed out is killed by the same path that then releases its
	// group, and that signal has not necessarily taken effect yet: looking once
	// finds somebody home and hands nothing back, and after a death there is no
	// next look - no stop is coming for this unit.
	deadline := time.Now().Add(cgroupSettle)
	for {
		alive, known := u.cgroupAlive()
		if !known || !alive || time.Now().After(deadline) {
			break
		}
		time.Sleep(cgroupPoll)
	}
	u.dropEmptyCgroup()
}

// dropEmptyCgroup removes the leaf once nothing of this unit is running inside
// it. It never signals: a process that outlived its main program keeps the
// group, because it is ours and it is still running - and if it then leaves on
// its own, the group waits for the next stop, or for the next start to reuse
// it. The kernel charges a task to its group until it is reaped, so the first
// attempt after a death can be refused with EBUSY; that clears in milliseconds.
func (u *Unit) dropEmptyCgroup() {
	if !u.hasCgroupLeaf() {
		return
	}
	deadline := time.Now().Add(cgroupSettle)
	for {
		if alive, known := u.cgroupAlive(); known && alive {
			return
		}
		err := cgroupTree().Remove(u.cgroupID())
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EBUSY) || time.Now().After(deadline) {
			return
		}
		time.Sleep(cgroupPoll)
	}
	u.mu.Lock()
	u.cgroupLeaf = false
	u.mu.Unlock()
}

// ControlGroup is systemd's ControlGroup property: where this unit's processes
// sit in the hierarchy, or empty when it has no group.
func (u *Unit) ControlGroup() string {
	if !u.hasCgroupLeaf() {
		return ""
	}
	return cgroupTree().GroupPath(u.cgroupID())
}

// CGroupPIDs is systemd's CGroupPIDs property: the processes the kernel
// attributes to this unit, or nil when there is no group to ask.
func (u *Unit) CGroupPIDs() []int { return u.cgroupMembers() }
