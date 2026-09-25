package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clearAmbientInvocationID removes an inherited INVOCATION_ID for the duration
// of a test. Without this the manager's own value can never be observed: initd
// inherits the daemon's environment, and a test run started from a service
// manager (or a shell that still carries one) arrives with the variable already
// set, so a passing assertion could be reading the ambient value rather than
// the one the manager provided.
func clearAmbientInvocationID(t *testing.T) {
	t.Helper()
	if _, ok := os.LookupEnv("INVOCATION_ID"); !ok {
		return
	}
	t.Setenv("INVOCATION_ID", "")
	if err := os.Unsetenv("INVOCATION_ID"); err != nil {
		t.Fatalf("unset INVOCATION_ID: %v", err)
	}
}

// systemd exports INVOCATION_ID to every service it starts, and supervised
// daemons rely on it to tell "I am the manager's child" apart from "some other
// process is already running me". A daemon that cannot see it reads its own
// unit as a conflict and refuses to start; the restart policy then starts it
// again and every attempt refuses the same way.
func TestChildGetsInvocationID(t *testing.T) {
	clearAmbientInvocationID(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "id")
	u := newTestUnit(t, "zzinv.service",
		"[Service]\nType=oneshot\nExecStart=/bin/sh -c 'printf %s \"$INVOCATION_ID\" > "+out+"'\n")
	if _, err := u.StartAndWait(true); err != nil {
		t.Fatalf("StartAndWait: %v", err)
	}
	waitForState(t, u, StateInactive)

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("child did not write INVOCATION_ID: %v", err)
	}
	id := strings.TrimSpace(string(raw))
	if id == "" {
		t.Fatal("INVOCATION_ID was empty in the child environment")
	}
	// systemd's form is a 32-char hex id, which is what newInvocationID mints.
	if len(id) != 32 {
		t.Errorf("INVOCATION_ID = %q (%d chars), want a 32-char id", id, len(id))
	}
}

// Each run is a distinct invocation: a restart must not hand the child the
// previous run's id, or a daemon correlating its own restarts would see one
// long-lived process where there were several.
func TestInvocationIDChangesBetweenRuns(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "ids")
	// A script file rather than a nested `sh -c`: the quoting through
	// ExecStart's shlex pass is not worth fighting for a test.
	script := filepath.Join(dir, "record.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$INVOCATION_ID\" >> "+out+"\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	u := newTestUnit(t, "zzinv2.service",
		"[Service]\nType=oneshot\nExecStart="+script+"\n")

	read := func() string {
		t.Helper()
		raw, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return strings.TrimSpace(string(raw))
	}

	if _, err := u.StartAndWait(true); err != nil {
		t.Fatalf("first StartAndWait: %v", err)
	}
	waitForState(t, u, StateInactive)
	// Restart needs the unit back at inactive; a oneshot that has run is.
	if err := u.Stop(2 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := u.StartAndWait(true); err != nil {
		t.Fatalf("second StartAndWait: %v", err)
	}
	waitForState(t, u, StateInactive)

	lines := strings.Split(read(), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected one id per run, got %d lines: %q", len(lines), lines)
	}
	if lines[0] == lines[1] {
		t.Errorf("both runs got INVOCATION_ID %q; a restart must mint a new one", lines[0])
	}
}

// initd inherits the daemon's whole environment, so a daemon launched under
// another service manager arrives carrying that manager's INVOCATION_ID. It
// must be replaced per run: passing it down gives every unit the same stale id,
// which is the very confusion the variable exists to prevent.
func TestStaleInheritedInvocationIDIsReplaced(t *testing.T) {
	t.Setenv("INVOCATION_ID", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	dir := t.TempDir()
	out := filepath.Join(dir, "id")
	u := newTestUnit(t, "zzinv4.service",
		"[Service]\nType=oneshot\nExecStart=/bin/sh -c 'printf %s \"$INVOCATION_ID\" > "+out+"'\n")
	if _, err := u.StartAndWait(true); err != nil {
		t.Fatalf("StartAndWait: %v", err)
	}
	waitForState(t, u, StateInactive)

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := strings.TrimSpace(string(raw))
	if strings.HasPrefix(got, "aaaaaaaa") {
		t.Errorf("child inherited the daemon's INVOCATION_ID %q; each run needs its own", got)
	}
	if len(got) != 32 {
		t.Errorf("INVOCATION_ID = %q (%d chars), want a fresh 32-char id", got, len(got))
	}
}

// A unit that pins INVOCATION_ID itself keeps its own value: the manager
// provides the variable, it does not own it.
func TestUnitCanOverrideInvocationID(t *testing.T) {
	clearAmbientInvocationID(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "id")
	u := newTestUnit(t, "zzinv3.service",
		"[Service]\nType=oneshot\nEnvironment=INVOCATION_ID=pinned-by-the-unit\n"+
			"ExecStart=/bin/sh -c 'printf %s \"$INVOCATION_ID\" > "+out+"'\n")
	if _, err := u.StartAndWait(true); err != nil {
		t.Fatalf("StartAndWait: %v", err)
	}
	waitForState(t, u, StateInactive)

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := strings.TrimSpace(string(raw)); got != "pinned-by-the-unit" {
		t.Errorf("INVOCATION_ID = %q, want the unit's own value", got)
	}
}
