package main

import (
	"os"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"initd/internal/ipc"
	"initd/internal/service"
)

func TestFormatSpanSystemdShape(t *testing.T) {
	cases := []struct {
		ago  time.Duration
		want string
	}{
		{14 * time.Millisecond, "14ms"},
		{999 * time.Millisecond, "999ms"},
		{46 * time.Second, "46s"},
		{90 * time.Second, "1min 30s"},
		{3540 * time.Second, "59min"},
		{9 * time.Hour, "9h"},
		{time.Hour + 20*time.Minute, "1h 20min"},
		{50 * time.Hour, "2days 2h"},
		{7 * 24 * time.Hour, "1week"},
		{8*24*time.Hour + 3*time.Hour, "1week 1day"},
		{0, "0s"},
		{time.Duration(-1), "0s"},
	}
	for _, c := range cases {
		if got := formatSpan(c.ago); got != c.want {
			t.Errorf("formatSpan(%v) = %q, want %q", c.ago, got, c.want)
		}
	}
}

func TestFormatSinceClampsUnusableStamps(t *testing.T) {
	// A stamp ahead of this clock, and one that was never set, must both
	// print a sane span instead of a negative one.
	if got := formatSince(monotonicNow() + time.Minute); got != "0s" {
		t.Errorf("formatSince(future) = %q, want 0s", got)
	}
	if got := formatSince(0); got != "0s" {
		t.Errorf("formatSince(unset) = %q, want 0s", got)
	}
}

func captureStatus(t *testing.T, status ipc.StatusData, enabled string) []string {
	t.Helper()
	original := os.Stdout
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = write
	printStatus(status, enabled, 0)
	write.Close()
	os.Stdout = original
	out := make([]byte, 1<<16)
	n, _ := read.Read(out)
	read.Close()
	return strings.Split(strings.TrimRight(string(out[:n]), "\n"), "\n")
}

// The header and the two state lines are what scripts grep; they must match
// systemd's text, including the bullet and the fifteen-column alignment.
func TestPrintStatusHeaderMatchesSystemd(t *testing.T) {
	now := time.Now()
	monotonic := monotonicNow() - 90*time.Second

	lines := captureStatus(t, ipc.StatusData{
		Name:                "app.service",
		Description:         "App",
		State:               service.StateActive,
		SubState:            "running",
		FragmentPath:        "/home/u/.config/systemd/user/app.service",
		StartedAt:           now,
		StartedAtMonotonic:  monotonic,
		FinishedAtMonotonic: 0,
	}, "static")
	if len(lines) < 3 {
		t.Fatalf("status printed %d lines: %q", len(lines), lines)
	}
	if lines[0] != "● app.service - App" {
		t.Errorf("header = %q", lines[0])
	}
	if lines[1] != "     Loaded: loaded (/home/u/.config/systemd/user/app.service; static)" {
		t.Errorf("loaded = %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "     Active: active (running) since ") || !strings.HasSuffix(lines[2], "; 1min 30s ago") {
		t.Errorf("active = %q", lines[2])
	}
	// The stamp must be systemd's weekday-first form. The zone is whatever
	// this machine is set to, so match the shape instead of naming one: "MST"
	// in Go's layout is a placeholder for the zone abbreviation, not the
	// literal text systemd prints, and asserting on it only passed on boxes
	// running in UTC or a zone that happens to abbreviate to MST.
	stamp := lines[2]
	if i := strings.Index(stamp, "since "); i >= 0 {
		stamp = stamp[i+len("since "):]
	}
	if j := strings.LastIndex(stamp, ";"); j >= 0 {
		stamp = stamp[:j]
	}
	if !weekdayFirstStamp.MatchString(strings.TrimSpace(stamp)) {
		t.Errorf("timestamp is not systemd's weekday-first form: %q", lines[2])
	}
}

// weekdayFirstStamp matches "Sat 2026-09-26 00:52:46 IST": a three-letter
// weekday, the date, the wall clock, then a zone abbreviation.
var weekdayFirstStamp = regexp.MustCompile(`^(Mon|Tue|Wed|Thu|Fri|Sat|Sun) \d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} \S+$`)

func TestPrintStatusInactiveAndFailed(t *testing.T) {
	inactive := captureStatus(t, ipc.StatusData{
		Name:        "app.service",
		Description: "App",
		State:       service.StateInactive,
		SubState:    "dead",
	}, "disabled")
	if inactive[0] != "○ app.service - App" {
		t.Errorf("inactive header = %q, want the empty bullet", inactive[0])
	}
	if inactive[2] != "     Active: inactive (dead)" {
		t.Errorf("inactive active = %q", inactive[2])
	}

	failed := captureStatus(t, ipc.StatusData{
		Name:                "app.service",
		Description:         "App",
		State:               service.StateFailed,
		SubState:            "failed",
		Result:              "exit-code",
		MainCode:            1,
		ExitCode:            3,
		ExecMainPID:         4242,
		ExecStart:           "/bin/sh -c exit 3",
		FinishedAt:          time.Now(),
		FinishedAtMonotonic: monotonicNow() - 14*time.Millisecond,
	}, "static")
	if failed[0] != "× app.service - App" {
		t.Errorf("failed header = %q, want the multiplication sign", failed[0])
	}
	if want := "     Active: failed (Result: exit-code)"; !strings.HasPrefix(failed[2], want) {
		t.Errorf("failed active = %q, want prefix %q", failed[2], want)
	}
	if failed[3] != "    Process: 4242 /bin/sh -c exit 3 (code=exited, status=3)" {
		t.Errorf("failed process = %q", failed[3])
	}
	if failed[4] != "   Main PID: 4242 (code=exited, status=3)" {
		t.Errorf("failed main pid = %q", failed[4])
	}
}

// A unit terminated by a signal - the notify timeout among them - is rendered
// with the signal name, not the 128+n status systemd only uses elsewhere.
func TestFormatExecOutcomeSignals(t *testing.T) {
	cases := []struct {
		name   string
		status ipc.StatusData
		want   string
	}{
		{"exited", ipc.StatusData{MainCode: 1, ExitCode: 3}, "(code=exited, status=3)"},
		{"chdir", ipc.StatusData{MainCode: 1, ExitCode: 200}, "(code=exited, status=200/CHDIR)"},
		{"exec", ipc.StatusData{MainCode: 1, ExitCode: 203}, "(code=exited, status=203/EXEC)"},
		{"group", ipc.StatusData{MainCode: 1, ExitCode: 216}, "(code=exited, status=216/GROUP)"},
		{"killed-term", ipc.StatusData{MainCode: 2, ExitCode: int(syscall.SIGTERM)}, "(code=killed, signal=TERM)"},
		{"killed-kill", ipc.StatusData{MainCode: 2, ExitCode: int(syscall.SIGKILL)}, "(code=killed, signal=KILL)"},
		{"dumped-segv", ipc.StatusData{MainCode: 3, ExitCode: int(syscall.SIGSEGV)}, "(code=dumped, signal=SEGV)"},
		{"none", ipc.StatusData{}, ""},
	}
	for _, c := range cases {
		if got := formatExecOutcome(c.status); got != c.want {
			t.Errorf("%s: formatExecOutcome = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPrintStatusKilledProcess(t *testing.T) {
	lines := captureStatus(t, ipc.StatusData{
		Name:                "app.service",
		Description:         "App",
		State:               service.StateFailed,
		SubState:            "failed",
		Result:              "timeout",
		MainCode:            2,
		ExitCode:            int(syscall.SIGTERM),
		ExecMainPID:         5150,
		ExecStart:           "/bin/sleep 200",
		FinishedAt:          time.Now(),
		FinishedAtMonotonic: monotonicNow() - 14*time.Millisecond,
	}, "static")
	if want := "     Active: failed (Result: timeout)"; !strings.HasPrefix(lines[2], want) {
		t.Errorf("active = %q, want prefix %q", lines[2], want)
	}
	if lines[3] != "    Process: 5150 /bin/sleep 200 (code=killed, signal=TERM)" {
		t.Errorf("process = %q", lines[3])
	}
}

// The cgroup stanza is where `status` answers who the kernel says runs for this
// unit. It is capped, because every line is a /proc read and a unit that forked
// a thousand ways would otherwise make `status` do exactly that.
func TestPrintStatusCGroupTree(t *testing.T) {
	mk := func(n int) []int {
		pids := make([]int, 0, n)
		for i := 0; i < n; i++ {
			pids = append(pids, 400000+i)
		}
		return pids
	}
	lines := captureStatus(t, ipc.StatusData{
		Name: "app.service", Description: "App", State: service.StateActive,
		SubState: "running", CGroup: "/initd/app.service", CGroupPIDs: mk(3),
	}, "static")
	if !strings.Contains(strings.Join(lines, "\n"),
		"     CGroup: /initd/app.service\n             ├─ 400000\n             ├─ 400001\n             └─ 400002") {
		t.Errorf("tree:\n%s", strings.Join(lines, "\n"))
	}

	capped := captureStatus(t, ipc.StatusData{
		Name: "app.service", Description: "App", State: service.StateActive,
		SubState: "running", CGroup: "/initd/app.service", CGroupPIDs: mk(25),
	}, "static")
	body := strings.Join(capped, "\n")
	if strings.Contains(body, "400020") {
		t.Errorf("the tree showed a process past the cap:\n%s", body)
	}
	if !strings.HasSuffix(capped[len(capped)-1], "└─ 5 more") {
		t.Errorf("last line = %q, want the count left out", capped[len(capped)-1])
	}
	if got := strings.Count(body, "4000"); got != 20 {
		t.Errorf("tree rendered %d members, want 20", got)
	}
}
