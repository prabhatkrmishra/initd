package service

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// procRoot is where process state is read from. Production is /proc; a test
// points INITD_PROC_ROOT at a fixture directory, because the scans here decide
// ownership and liveness from kernel-provided files that a test cannot create
// under the real /proc without root.
func procRoot() string {
	if p := strings.TrimSpace(os.Getenv("INITD_PROC_ROOT")); p != "" {
		return p
	}
	return "/proc"
}

// liveness is the kernel's answer about one PID. It carries three values
// because the daemon runs under hidepid=2 on the target: a process owned by
// another user is not merely unreadable there, it is invisible, and answering
// "dead" to that reports a possibly-running service as stopped.
type liveness int

const (
	livenessAlive liveness = iota
	livenessDead
	// livenessUnknown means the process exists but this process may not look
	// at it (EPERM from kill(), or an entry /proc is hiding).
	livenessUnknown
)

// notDead is what anything waiting for a process to go away must ask: unknown
// is not evidence of death.
func (l liveness) notDead() bool { return l != livenessDead }

// checkLiveness is the single place that decides whether a PID runs.
func checkLiveness(pid int) liveness {
	if pid <= 0 {
		return livenessDead
	}
	err := syscall.Kill(pid, 0)
	if err == syscall.EPERM {
		// The process is there, in a user we can neither signal nor inspect.
		return livenessUnknown
	}
	if err != nil {
		return livenessDead
	}
	// kill() was accepted, so the process exists and is ours. A zombie still
	// answers that but is not running, so read the state field: it follows the
	// parenthesised comm, which may contain spaces and closing parens.
	data, rerr := os.ReadFile(filepath.Join(procRoot(), strconv.Itoa(pid), "stat"))
	if rerr != nil {
		// Alive as far as the kernel will tell us; /proc is just not there to
		// look at (unmounted, or hiding this entry). That is not a death.
		return livenessAlive
	}
	if idx := strings.LastIndexByte(string(data), ')'); idx >= 0 && idx+2 < len(data) {
		if data[idx+2] == 'Z' {
			return livenessDead
		}
	}
	return livenessAlive
}

// procUIDs returns the real and effective UID of a process. ok is false when
// the entry is invisible to us, which under hidepid=2 is the ordinary answer
// for any process we do not own.
func procUIDs(pid int) (real, effective uint32, ok bool) {
	data, err := os.ReadFile(filepath.Join(procRoot(), strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		fields := strings.Fields(line[len("Uid:"):])
		if len(fields) < 2 {
			return 0, 0, false
		}
		r, err := strconv.ParseUint(fields[0], 10, 32)
		if err != nil {
			return 0, 0, false
		}
		e, err := strconv.ParseUint(fields[1], 10, 32)
		if err != nil {
			return 0, 0, false
		}
		return uint32(r), uint32(e), true
	}
	return 0, 0, false
}

// procOwnedBy reports whether pid is a process of the unit that runs as uid.
// An entry that cannot be read is deliberately answered false: adopting a
// process we cannot see is how a restart takes over someone else's daemon and
// then finds it cannot signal it.
func procOwnedBy(pid int, uid uint32) bool {
	real, effective, ok := procUIDs(pid)
	return ok && (real == uid || effective == uid)
}

// procPIDs lists the numeric entries of the proc fs. Nothing is decided by the
// directory-entry type: a name that parses as a positive number is the only
// property a process directory has anyway. A /proc that cannot be read yields
// nothing, which callers must treat as "found no candidate" and never as
// "nothing is running".
func procPIDs() []int {
	entries, err := os.ReadDir(procRoot())
	if err != nil {
		return nil
	}
	pids := make([]int, 0, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// groupHasOwnedMember reports whether process group gid still holds a process
// this unit owns. A recorded group ID is only a number: once our starter has
// exited, the kernel may hand that number to an unrelated process group, and
// Kill(-gid) would then reach strangers.
func groupHasOwnedMember(gid int, uid uint32) bool {
	if gid <= 0 {
		return false
	}
	for _, pid := range procPIDs() {
		if member, err := syscall.Getpgid(pid); err == nil && member == gid &&
			procOwnedBy(pid, uid) && checkLiveness(pid) == livenessAlive {
			return true
		}
	}
	return false
}

// clockTicks is the divisor for /proc/<pid>/stat's start time. Every Linux
// arch the target set uses 100; the kernel hard-codes it for procfs output.
const clockTicks = 100

// procStartTime is the wall-clock instant a process began running, from the
// clock ticks since boot in /proc/<pid>/stat plus the boot time in
// /proc/stat. ok is false when either half cannot be read, which includes
// every test fixture - callers must treat that as "no evidence", not as a
// rejection.
func procStartTime(pid int) (time.Time, bool) {
	data, err := os.ReadFile(filepath.Join(procRoot(), strconv.Itoa(pid), "stat"))
	if err != nil {
		return time.Time{}, false
	}
	// Field 22 is the start time. The fields after the last ')' begin with
	// field 3 (state), so it is index 19 of that tail.
	idx := strings.LastIndexByte(string(data), ')')
	if idx < 0 {
		return time.Time{}, false
	}
	fields := strings.Fields(string(data)[idx+1:])
	if len(fields) < 20 {
		return time.Time{}, false
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	boot, err := os.ReadFile(filepath.Join(procRoot(), "stat"))
	if err != nil {
		return time.Time{}, false
	}
	var btime int64 = -1
	for _, line := range strings.Split(string(boot), "\n") {
		if !strings.HasPrefix(line, "btime ") {
			continue
		}
		if v, err := strconv.ParseInt(strings.TrimSpace(line[len("btime "):]), 10, 64); err == nil {
			btime = v
		}
		break
	}
	if btime < 0 {
		return time.Time{}, false
	}
	// Split the ticks rather than multiplying them by time.Second: that
	// product overflows int64 once a box has been up for about three years,
	// and a wrapped start time would be read as evidence about a process.
	return time.Unix(btime+int64(ticks/clockTicks),
		int64(ticks%clockTicks)*int64(time.Second/clockTicks)), true
}

// freshProcessMargin is how far apart a process's start and its pid file's last
// write must disagree before that counts as evidence. Smaller gaps are the
// signature of a clock step: /proc start times are reconstructed from the boot
// time, so a forward jump - the network time sync every Android box does -
// moves them away from mtimes written before it, and a daemon that wrote its
// file at startup would look like a stranger that reused the number.
const freshProcessMargin = 10 * time.Minute

// pidFileNamesFreshProcess reports whether a PIDFile= entry points at a
// process that began long after the file was last written - the signature of a
// stale pid file whose number the kernel has already handed to somebody else.
// A daemon writes its own file, so it cannot start later than that write.
//
// Missing evidence is answered false: a process that cannot be timed (no
// /proc, a test fixture) is judged as it was before this check existed, since
// refusing to adopt it would break services that work today.
func pidFileNamesFreshProcess(path string, pid int) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	start, ok := procStartTime(pid)
	if !ok {
		return false
	}
	return start.After(info.ModTime().Add(freshProcessMargin))
}

// expectedUID is the UID this unit's processes run as: User= when the unit
// names one, otherwise the daemon's own. Resolving a name reads the passwd
// database, so the answer is memoised per unit and dropped when a
// daemon-reload changes User=. It keeps its own lock so that no caller has to
// think about lock order with mu - the scans run from the stop path, which
// takes mu around short state reads.
func (u *Unit) expectedUID() uint32 {
	name := strings.TrimSpace(u.GetConfig().Service.User)
	self := uint32(os.Getuid())
	if name == "" {
		u.uidMu.Lock()
		u.uidCacheName, u.uidCacheUID = "", self
		u.uidMu.Unlock()
		return self
	}
	u.uidMu.Lock()
	cached, valid := u.uidCacheUID, u.uidCacheName == name
	u.uidMu.Unlock()
	if valid {
		return cached
	}
	uid, _, err := lookupUser(name)
	if err != nil {
		// A unit whose user cannot be resolved cannot start either; matching
		// the daemon's own processes is the least wrong answer.
		return self
	}
	u.uidMu.Lock()
	u.uidCacheUID, u.uidCacheName = uid, name
	u.uidMu.Unlock()
	return uid
}
