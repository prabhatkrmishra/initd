package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/user"
	"strings"
	"testing"
	"time"
)

// stubLogin1 replaces the two logind queries for one test, so the handlers,
// the table formatter and the JSON encoder can be exercised without a bus.
func stubLogin1(t *testing.T, sessions []loginSession, serr error, users []loginUser, uerr error) {
	t.Helper()
	oldSessions, oldUsers := fetchLoginSessions, fetchLoginUsers
	fetchLoginSessions = func() ([]loginSession, error) { return sessions, serr }
	fetchLoginUsers = func() ([]loginUser, error) { return users, uerr }
	t.Cleanup(func() { fetchLoginSessions, fetchLoginUsers = oldSessions, oldUsers })
}

func captureStdout(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = old
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	return string(out), code
}

func TestListSessionsUsesLogind(t *testing.T) {
	stubLogin1(t, []loginSession{
		{ID: "c2", UID: 1000, User: "pkm", TTY: "pts/0", State: "active"},
		{ID: "c26", UID: 1000, User: "pkm", Idle: true, Since: "2min ago"},
	}, nil, nil, nil)

	out, code := captureStdout(t, func() int { return handleListSessions(loginctlOptions{}, nil) })
	if code != 0 {
		t.Fatalf("list-sessions = %d", code)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %q", lines)
	}
	if got := strings.Fields(lines[0]); strings.Join(got, " ") != "SESSION UID USER SEAT TTY STATE IDLE SINCE" {
		t.Fatalf("header = %q", lines[0])
	}
	if got := strings.Fields(lines[1]); strings.Join(got, " ") != "c2 1000 pkm - pts/0 active no -" {
		t.Fatalf("first row = %q", lines[1])
	}
	// Empty seat and the idle SINCE text must both survive formatting.
	if got := strings.Fields(lines[2]); strings.Join(got, " ") != "c26 1000 pkm - - - yes 2min ago" {
		t.Fatalf("second row = %q", lines[2])
	}
	if lines[3] != "2 sessions listed." {
		t.Fatalf("footer = %q", lines[3])
	}

	out, _ = captureStdout(t, func() int {
		return handleListSessions(loginctlOptions{noLegend: true}, nil)
	})
	if strings.HasPrefix(out, "SESSION") {
		t.Fatalf("--no-legend kept the header: %q", out)
	}
}

func TestListSessionsJSONFromLogind(t *testing.T) {
	stubLogin1(t, []loginSession{
		{ID: "c2", UID: 1000, User: "pkm", Seat: "seat0", TTY: "tty1", State: "active"},
	}, nil, nil, nil)

	out, code := captureStdout(t, func() int {
		return handleListSessions(loginctlOptions{jsonMode: "short"}, nil)
	})
	if code != 0 {
		t.Fatalf("list-sessions --json = %d", code)
	}
	var rows []struct {
		Session string `json:"session"`
		UID     uint32 `json:"uid"`
		User    string `json:"user"`
		Seat    string `json:"seat"`
		TTY     string `json:"tty"`
		State   string `json:"state"`
		Idle    bool   `json:"idle"`
		Since   string `json:"since"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("json: %v (%q)", err, out)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	if r := rows[0]; r.Session != "c2" || r.UID != 1000 || r.Seat != "seat0" || r.Since != "-" {
		t.Fatalf("row = %+v", r)
	}
}

func TestListSessionsWithoutLogind(t *testing.T) {
	stubLogin1(t, nil, errors.New("no bus"), nil, nil)

	out, code := captureStdout(t, func() int { return handleListSessions(loginctlOptions{}, nil) })
	if code != 0 {
		t.Fatalf("list-sessions without logind = %d", code)
	}
	if !strings.Contains(out, "0 sessions listed.") {
		t.Fatalf("output = %q", out)
	}

	out, _ = captureStdout(t, func() int {
		return handleListSessions(loginctlOptions{jsonMode: "short"}, nil)
	})
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("json = %q, want []", out)
	}
}

func TestListUsersUsesLogindAndLinger(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	oldDir := systemLingerDir
	systemLingerDir = t.TempDir()
	t.Cleanup(func() { systemLingerDir = oldDir })

	me, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	if err := setLinger(me.Username); err != nil {
		t.Fatalf("setLinger: %v", err)
	}

	// logind knows root; the current user is only in the linger store, which
	// is what the local shim writes when it enables linger.
	stubLogin1(t, nil, nil, []loginUser{
		{UID: 0, Name: "root", Linger: "no", State: "online"},
	}, nil)

	out, code := captureStdout(t, func() int { return handleListUsers(loginctlOptions{}, nil) })
	if code != 0 {
		t.Fatalf("list-users = %d", code)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %q", lines)
	}
	if got := strings.Fields(lines[1]); strings.Join(got, " ") != "0 root no online" {
		t.Fatalf("root row = %q", lines[1])
	}
	if got := strings.Fields(lines[2]); strings.Join(got, " ") != me.Uid+" "+me.Username+" yes lingering" {
		t.Fatalf("linger row = %q", lines[2])
	}
	if lines[3] != "2 users listed." {
		t.Fatalf("footer = %q", lines[3])
	}
}

func TestListUsersWithoutLogindKeepsLinger(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	oldDir := systemLingerDir
	systemLingerDir = t.TempDir()
	t.Cleanup(func() { systemLingerDir = oldDir })

	me, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	if err := setLinger(me.Username); err != nil {
		t.Fatalf("setLinger: %v", err)
	}
	stubLogin1(t, nil, nil, nil, errors.New("no bus"))

	out, code := captureStdout(t, func() int { return handleListUsers(loginctlOptions{}, nil) })
	if code != 0 {
		t.Fatalf("list-users without logind = %d", code)
	}
	if !strings.Contains(out, me.Username) || !strings.Contains(out, "1 users listed.") {
		t.Fatalf("output = %q", out)
	}
}

func TestShortDuration(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Second, "5s"},
		{90 * time.Second, "1min 30s"},
		{2 * time.Minute, "2min"},
		{3*time.Hour + 5*time.Minute, "3h 5min"},
		{49 * time.Hour, "2d"},
	} {
		if got := shortDuration(tc.d); got != tc.want {
			t.Errorf("shortDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
	if got := idleSince(0); got != "-" {
		t.Errorf("idleSince(0) = %q, want -", got)
	}
}
