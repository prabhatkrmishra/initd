package service

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"initd/internal/logging"
)

// Transient scope units (Type=scope), the unit kind logind uses for user
// sessions. A scope owns a set of processes it never exec'd: it goes active
// when the processes are placed into it and inactive when they are all gone.
// There is no ExecStart, no main PID, and stop means SIGTERM the group and
// wait, then SIGKILL whatever survived.

func (u *Unit) isScope() bool {
	return strings.EqualFold(strings.TrimSpace(u.GetConfig().Service.Type), "scope")
}

// SetScopeEmptyHandler registers the callback fired when the scope's last
// process exits, so the manager can unload the transient unit.
func (u *Unit) SetScopeEmptyHandler(h func(string)) {
	u.mu.Lock()
	u.onScopeEmpty = h
	u.mu.Unlock()
}

// ActivateScope brings the scope up around the given PIDs. Placement happens
// in the unit's cgroup leaf when the box provides one; liveness fall back to
// the raw PIDs otherwise.
func (u *Unit) ActivateScope(pids []int, remainAfterExit bool) error {
	u.mu.Lock()
	if state := u.Runtime.State; state == StateActive || state == StateActivating {
		u.mu.Unlock()
		return fmt.Errorf("unit %s is already active", u.GetConfig().Name)
	}
	u.Runtime.State = StateActivating
	u.Runtime.MainPID = 0
	u.Runtime.ExecMainPID = 0
	u.Runtime.Result = ""
	u.Runtime.LastError = ""
	u.Runtime.StartedAt = time.Now()
	u.Runtime.StartedAtMonotonic = logging.MonotonicNow()
	u.Runtime.FinishedAt = time.Time{}
	u.Runtime.FinishedAtMonotonic = 0
	u.pgid = 0
	u.startToken++
	u.scopePIDs = append([]int(nil), pids...)
	u.scopeRemainAfterExit = remainAfterExit
	token := u.startToken
	u.mu.Unlock()

	u.prepareCgroup()
	for _, pid := range pids {
		u.placeInCgroup(pid)
	}

	u.mu.Lock()
	if u.startToken != token {
		u.mu.Unlock()
		return fmt.Errorf("unit %s was superseded while it was being activated", u.GetConfig().Name)
	}
	u.Runtime.State = StateActive
	u.mu.Unlock()
	go u.watchScopeEmpty(token)
	return nil
}

// scopeAlive reports whether any process the scope was created with - or that
// was placed in its group - is still around.
func (u *Unit) scopeAlive() bool {
	if members := u.cgroupMembers(); members != nil {
		for _, pid := range members {
			if pid != os.Getpid() && checkLiveness(pid) != livenessDead {
				return true
			}
		}
		return false
	}
	u.mu.Lock()
	pids := append([]int(nil), u.scopePIDs...)
	u.mu.Unlock()
	for _, pid := range pids {
		if checkLiveness(pid) != livenessDead {
			return true
		}
	}
	return false
}

// watchScopeEmpty flips the scope to inactive once its last process is gone.
// RemainAfterExit scopes keep polling but never deactivate on their own.
func (u *Unit) watchScopeEmpty(token int) {
	for {
		time.Sleep(150 * time.Millisecond)
		u.mu.Lock()
		if u.startToken != token {
			u.mu.Unlock()
			return
		}
		state := u.Runtime.State
		stopReq := u.stopRequested
		remain := u.scopeRemainAfterExit
		u.mu.Unlock()

		switch state {
		case StateActive:
		default:
			return
		}
		if stopReq {
			// The stop path drives the transition itself.
			return
		}
		if remain {
			continue
		}
		if u.scopeAlive() {
			continue
		}
		u.mu.Lock()
		if u.startToken == token && u.Runtime.State == StateActive && !u.stopRequested {
			u.Runtime.State = StateInactive
			u.Runtime.Result = "success"
			u.Runtime.ExitCode = 0
			u.Runtime.LastError = ""
			u.Runtime.MainPID = 0
			u.Runtime.ExecMainPID = 0
			u.Runtime.FinishedAt = time.Now()
			u.Runtime.FinishedAtMonotonic = logging.MonotonicNow()
		}
		u.mu.Unlock()
		u.releaseEmptyCgroup()
		u.scopeEmptied()
		return
	}
}

// stopScope terminates everything in the scope: SIGTERM, a bounded wait, then
// SIGKILL. It never runs ExecStop, which a scope never had.
func (u *Unit) stopScope(timeout time.Duration) error {
	u.mu.Lock()
	if u.Runtime.State == StateInactive {
		u.mu.Unlock()
		return nil
	}
	u.stopRequested = true
	u.startToken++ // invalidate the empty watcher; stop drives the transition
	u.Runtime.State = StateStopping
	u.mu.Unlock()

	u.prepareCgroup()
	if sent := u.cgroupSweep(syscall.SIGTERM); sent == 0 {
		u.mu.Lock()
		pids := append([]int(nil), u.scopePIDs...)
		u.mu.Unlock()
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !u.scopeAlive() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if u.scopeAlive() {
		if sent := u.cgroupSweep(syscall.SIGKILL); sent == 0 {
			u.mu.Lock()
			pids := append([]int(nil), u.scopePIDs...)
			u.mu.Unlock()
			for _, pid := range pids {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		killDeadline := time.Now().Add(2 * time.Second)
		for u.scopeAlive() && time.Now().Before(killDeadline) {
			time.Sleep(25 * time.Millisecond)
		}
	}

	u.mu.Lock()
	u.Runtime.State = StateInactive
	u.Runtime.Result = "success"
	u.Runtime.ExitCode = 0
	u.Runtime.LastError = ""
	u.Runtime.MainPID = 0
	u.Runtime.ExecMainPID = 0
	u.Runtime.FinishedAt = time.Now()
	u.Runtime.FinishedAtMonotonic = logging.MonotonicNow()
	u.mu.Unlock()
	u.finishCgroupStop()
	u.scopeEmptied()
	return nil
}

// scopeEmptied tells the manager the transient unit may be unloaded.
func (u *Unit) scopeEmptied() {
	u.mu.Lock()
	h := u.onScopeEmpty
	name := u.GetConfig().Name
	u.mu.Unlock()
	if h != nil {
		h(name)
	}
}
