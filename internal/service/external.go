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

// FindExternalPID scans /proc for a live process whose argv0 basename matches
// the unit's ExecStart binary. It returns the PID and full cmdline, or 0,""
// when nothing matches. The calling unit's own MainPID is skipped so a stale
// PID is never reported as external.
func (u *Unit) FindExternalPID() (int, string) {
	execStart := ""
	selfPID := 0
	u.mu.Lock()
	if u.Config != nil {
		execStart = u.Config.Service.ExecStart
	}
	selfPID = u.Runtime.MainPID
	u.mu.Unlock()

	candidates := execStartBinaries(execStart)
	if len(candidates) == 0 {
		return 0, ""
	}
	want := candidates[0]

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
		// Match basename exactly, or full path suffix for absolute ExecStart.
		if base == want {
			return pid, strings.ReplaceAll(strings.TrimRight(string(cmdline), "\x00"), "\x00", " ")
		}
		for _, c := range candidates[1:] {
			if argv0 == c || strings.HasSuffix(argv0, "/"+want) {
				return pid, strings.ReplaceAll(strings.TrimRight(string(cmdline), "\x00"), "\x00", " ")
			}
		}
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
	if pid, _ := u.FindExternalPID(); pid > 0 {
		return StateActive, pid
	}
	return snap.State, snap.MainPID
}
