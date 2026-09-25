package service

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/shlex"
)

// execStartBinaries extracts candidate binary basenames from an ExecStart line.
// It strips systemd prefixes (@, :, !, +, -), env assignments (KEY=val) and
// returns the basename of the executable plus the full argv0 for fallback
// matching. Empty ExecStart yields no candidates.
func execStartBinaries(execStart string) []string {
	s := strings.TrimSpace(execStart)
	for len(s) > 0 && strings.Contains("@:!+-", s[:1]) {
		s = strings.TrimSpace(s[1:])
	}
	if s == "" {
		return nil
	}
	args, err := shlex.Split(s)
	if err != nil || len(args) == 0 {
		// Fall back to naive fields split so a malformed line still probes.
		args = strings.Fields(s)
		if len(args) == 0 {
			return nil
		}
	}
	// Skip leading VAR=value assignments.
	idx := 0
	for idx < len(args) && strings.Contains(args[idx], "=") && !strings.HasPrefix(args[idx], "/") {
		// Heuristic: KEY=val has no slash before '='.
		eq := strings.Index(args[idx], "=")
		slash := strings.Index(args[idx], "/")
		if eq > 0 && (slash == -1 || slash > eq) {
			idx++
			continue
		}
		break
	}
	if idx >= len(args) {
		return nil
	}
	bin := args[idx]
	base := filepath.Base(bin)
	if base == "" || base == "." || base == "/" {
		return nil
	}
	if base == bin {
		return []string{base}
	}
	return []string{base, bin}
}

// execStartArgv parses an ExecStart line into argv after stripping systemd
// prefixes and leading VAR= assignments. Returns nil for empty lines.
func execStartArgv(execStart string) []string {
	s := strings.TrimSpace(execStart)
	for len(s) > 0 && strings.Contains("@:!+-", s[:1]) {
		s = strings.TrimSpace(s[1:])
	}
	if s == "" {
		return nil
	}
	args, err := shlex.Split(s)
	if err != nil || len(args) == 0 {
		args = strings.Fields(s)
		if len(args) == 0 {
			return nil
		}
	}
	idx := 0
	for idx < len(args) && strings.Contains(args[idx], "=") && !strings.HasPrefix(args[idx], "/") {
		eq := strings.Index(args[idx], "=")
		slash := strings.Index(args[idx], "/")
		if eq > 0 && (slash == -1 || slash > eq) {
			idx++
			continue
		}
		break
	}
	if idx >= len(args) {
		return nil
	}
	return args[idx:]
}

// FindExternalPID scans /proc for a live process whose argv0 basename matches
// the unit's ExecStart binary. It returns the PID and full cmdline, or 0,""
// when nothing matches. The calling unit's own MainPID is skipped so a stale
// PID is never reported as external, and a process owned by any other UID is
// skipped too: without that, one visible copy of a popular command (a shell
// running the same binary as another user's service) could be handed over as
// this unit's daemon, which we then cannot signal.
func (u *Unit) FindExternalPID() (int, string) {
	execStart := ""
	selfPID := 0
	u.mu.Lock()
	if u.GetConfig() != nil {
		execStart = u.GetConfig().Service.ExecStart
	}
	selfPID = u.Runtime.MainPID
	u.mu.Unlock()

	candidates := execStartBinaries(execStart)
	if len(candidates) == 0 {
		return 0, ""
	}
	want := candidates[0]
	wantArgv := execStartArgv(execStart)
	var wantArgs []string
	if len(wantArgv) > 1 {
		wantArgs = wantArgv[1:]
		// Mirror runStartSequence argv[0] override: ExecStart=@/bin/foo
		// @custom ... runs with argv[0]=custom, so drop the @token from
		// the args we require in /proc.
		if len(wantArgs) > 0 && len(wantArgs[0]) > 0 && wantArgs[0][0] == '@' {
			wantArgs = wantArgs[1:]
		}
	}

	matches := func(pid int) (string, bool) {
		cmdline, err := os.ReadFile(filepath.Join(procRoot(), strconv.Itoa(pid), "cmdline"))
		if err != nil || len(cmdline) == 0 {
			return "", false
		}
		// cmdline is NUL-separated; argv0 is up to first NUL.
		argv0 := string(cmdline)
		if i := strings.IndexByte(argv0, 0); i >= 0 {
			argv0 = argv0[:i]
		}
		argv0 = strings.TrimSpace(argv0)
		if argv0 == "" {
			return "", false
		}
		base := filepath.Base(argv0)
		matched := false
		// Match basename exactly, or full path suffix for absolute ExecStart.
		if base == want {
			matched = true
		} else {
			for _, c := range candidates[1:] {
				if argv0 == c || strings.HasSuffix(argv0, "/"+want) {
					matched = true
					break
				}
			}
		}
		if !matched {
			return "", false
		}
		// When ExecStart carries args (e.g. "sleep infinity" vs "sleep 10"),
		// require them: a basename-only match would let two units sharing
		// a binary claim each other's processes.
		if len(wantArgs) > 0 {
			procArgv := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
			if len(procArgv)-1 < len(wantArgs) {
				return "", false
			}
			for i, wa := range wantArgs {
				if procArgv[i+1] != wa {
					return "", false
				}
			}
		}
		return strings.ReplaceAll(strings.TrimRight(string(cmdline), "\x00"), "\x00", " "), true
	}
	describe := func(pid int) (int, string, bool) {
		if pid <= 1 || pid == os.Getpid() || pid == selfPID {
			return 0, "", false
		}
		line, ok := matches(pid)
		if !ok {
			return 0, "", false
		}
		// A process we do not own is not ours to supervise, and under hidepid=2
		// it is not even readable - so an unreadable owner rejects. Asked only
		// after the command line matches, which throws away most of the box.
		if !procOwnedBy(pid, u.expectedUID()) {
			return 0, "", false
		}
		return pid, line, true
	}

	// A process the kernel already attributes to this unit beats a search: the
	// scan below can only say "something runs this command line", which is not
	// the same as "this is the daemon".
	if members := u.cgroupMembers(); members != nil {
		for _, pid := range members {
			if found, line, ok := describe(pid); ok {
				return found, line
			}
		}
	}
	for _, pid := range procPIDs() {
		if found, line, ok := describe(pid); ok {
			return found, line
		}
	}
	return 0, ""
}

// EffectiveState returns the snapshot state, upgraded to active when the
// supervisor thinks the unit is inactive but its ExecStart binary is running
// externally (SysV / manual / nohup start, e.g. a daemon launched via
// /etc/init.d or nohup outside initd supervision). It also returns
// the external PID (0 when none).
//
// Matching is argv-based over the /proc entries this unit could own, with the
// unit's own cgroup consulted first where one exists: membership is process
// identity rather than a command line that any other process may copy, so a
// group's member is trusted and only a unit without a group falls back to the
// scan. Where there is no group the scan stays a deliberately coarse
// compatibility fallback - two units with byte-identical commands can still
// claim the same process, and a hand-run copy of a command is indistinguishable
// from the real daemon. Treat external matches as advisory, never as
// supervision.
func (u *Unit) EffectiveState() (State, int) {
	snap := u.Snapshot()
	if snap.State == StateActive || snap.State == StateActivating {
		return snap.State, snap.MainPID
	}
	// Only Inactive units can be upgraded by an external process. Failed
	// must stay failed until reset (otherwise a crash loop looks active),
	// and Stopping must not resurrect mid-stop.
	if snap.State != StateInactive {
		return snap.State, snap.MainPID
	}
	// A unit this daemon deliberately stopped stays stopped. The scan below
	// cannot tell "someone else runs this command" from "the copy we just
	// killed has not left /proc yet" or "a second unit shares the exact same
	// ExecStart", and answering active made `systemctl stop X && systemctl
	// is-active X` report active - so every restart script that gates on it
	// skipped the start.
	if u.StopRequested() {
		return snap.State, snap.MainPID
	}
	if pid, _ := u.FindExternalPID(); pid > 0 {
		return StateActive, pid
	}
	return snap.State, snap.MainPID
}
