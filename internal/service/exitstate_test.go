package service

import (
	"testing"
	"time"
)

// A unit that has exited has nothing running. Reporting it as "active
// (running)" with MainPID=0 is a lie a client cannot act on: every service
// manager is told a start finished by polling for a live process, and a unit
// stuck in this state can never satisfy that poll, so it reads as permanently
// one step behind its own restart.
//
// Reproduced with a Type=simple unit that exits cleanly, which is what a
// daemon does when it is asked to hand over to its successor.
func TestSimpleUnitThatExitsCleanlyGoesInactive(t *testing.T) {
	u := newTestUnit(t, "zzexit.service", "[Service]\nType=simple\nExecStart=/bin/sleep 0.2\n")
	if _, err := u.StartAndWait(true); err != nil {
		t.Fatalf("StartAndWait: %v", err)
	}

	// It has to reach active first, or this proves nothing about the exit.
	waitForState(t, u, StateActive)

	waitForState(t, u, StateInactive)
	snap := u.Snapshot()
	if snap.State != StateInactive {
		t.Errorf("state after a clean exit = %q, want inactive", snap.State)
	}
	if snap.MainPID != 0 {
		t.Errorf("MainPID = %d after the process exited, want 0", snap.MainPID)
	}
	if snap.Result != "success" {
		t.Errorf("Result = %q, want success: a clean exit is not a failure", snap.Result)
	}
	if snap.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", snap.ExitCode)
	}
}

// A oneshot that exits cleanly is inactive too, and has been all along. Kept
// here so the fix above cannot be mistaken for a behaviour change to oneshots.
func TestOneshotWithoutRemainAfterExitGoesInactive(t *testing.T) {
	u := newTestUnit(t, "zzexit1.service", "[Service]\nType=oneshot\nExecStart=/bin/true\n")
	if _, err := u.StartAndWait(true); err != nil {
		t.Fatalf("StartAndWait: %v", err)
	}
	waitForState(t, u, StateInactive)
	if got := u.Snapshot().State; got != StateInactive {
		t.Errorf("state = %q, want inactive", got)
	}
}

// RemainAfterExit is the one case where a unit with no process is genuinely
// active: systemd's "active (exited)". It must survive the same exit path.
func TestOneshotWithRemainAfterExitStaysActive(t *testing.T) {
	u := newTestUnit(t, "zzexit2.service",
		"[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/bin/true\n")
	if _, err := u.StartAndWait(true); err != nil {
		t.Fatalf("StartAndWait: %v", err)
	}
	waitForState(t, u, StateActive)
	snap := u.Snapshot()
	if snap.State != StateActive {
		t.Errorf("state = %q, want active (exited)", snap.State)
	}
	if snap.MainPID != 0 {
		t.Errorf("MainPID = %d, want 0 for active (exited)", snap.MainPID)
	}
}

// A failing simple unit still fails: the fix is only about the clean exit.
func TestSimpleUnitThatFailsStillGoesFailed(t *testing.T) {
	u := newTestUnit(t, "zzexit3.service", "[Service]\nType=simple\nExecStart=/bin/sh -c 'exit 3'\n")
	if _, err := u.StartAndWait(true); err != nil {
		t.Fatalf("StartAndWait: %v", err)
	}
	waitForState(t, u, StateFailed)
	snap := u.Snapshot()
	if snap.State != StateFailed {
		t.Errorf("state = %q, want failed", snap.State)
	}
	if snap.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", snap.ExitCode)
	}
}

// A clean exit is still a clean exit when the unit was left active by a
// previous run of a start that had not yet been reaped, which is the ordering
// the reported failure actually had: Restart=always restarting a unit whose
// last process had just drained.
func TestRestartAfterCleanExitReachesActiveAgain(t *testing.T) {
	u := newTestUnit(t, "zzexit4.service", "[Service]\nType=simple\nExecStart=/bin/sleep 0.2\n")
	if _, err := u.StartAndWait(true); err != nil {
		t.Fatalf("first StartAndWait: %v", err)
	}
	waitForState(t, u, StateInactive)

	if err := u.Stop(2 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := u.StartAndWait(true); err != nil {
		t.Fatalf("second StartAndWait: %v", err)
	}
	waitForState(t, u, StateActive)
	if snap := u.Snapshot(); snap.MainPID == 0 {
		t.Error("MainPID = 0 while active; a live process must be reported")
	}
	// Leave the unit settled for the next assertion.
	waitForState(t, u, StateInactive)
}
