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
// PID is never reported as external.
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

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, ""
	}
	self := os.Getpid()
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || pid == self || pid == selfPID {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || len(cmdline) == 0 {
			continue
		}
		// cmdline is NUL-separated; argv0 is up to first NUL.
		argv0 := string(cmdline)
		if i := strings.IndexByte(argv0, 0); i >= 0 {
			argv0 = argv0[:i]
		}
		argv0 = strings.TrimSpace(argv0)
		if argv0 == "" {
			continue
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
			continue
		}
		// When ExecStart carries args (e.g. "sleep infinity" vs "sleep 10"),
		// require them: a basename-only match would let two units sharing
		// a binary claim each other's processes.
		if len(wantArgs) > 0 {
			procArgv := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
			if len(procArgv)-1 < len(wantArgs) {
				continue
			}
			ok := true
			for i, wa := range wantArgs {
				if procArgv[i+1] != wa {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
		}
		return pid, strings.ReplaceAll(strings.TrimRight(string(cmdline), "\x00"), "\x00", " ")
	}
	return 0, ""
}

// EffectiveState returns the snapshot state, upgraded to active when the
// supervisor thinks the unit is inactive but its ExecStart binary is running
// externally (SysV / manual / nohup start, e.g. a daemon launched via
// /etc/init.d or nohup outside initd supervision). It also returns
// the external PID (0 when none).
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
	if pid, _ := u.FindExternalPID(); pid > 0 {
		return StateActive, pid
	}
	return snap.State, snap.MainPID
}
