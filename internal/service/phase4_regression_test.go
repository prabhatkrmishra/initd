package service

import (
	"os/exec"
	"testing"
	"time"

	"initd/internal/parser"
)

func TestExternalArgsMismatch(t *testing.T) {
	// Start two sleep processes with different args; units requiring
	// specific args must not cross-match by basename alone.
	a := exec.Command("sleep", "300")
	if err := a.Start(); err != nil {
		t.Skip("sleep not available")
	}
	defer a.Process.Kill()
	b := exec.Command("sleep", "301")
	if err := b.Start(); err != nil {
		t.Skip("sleep not available")
	}
	defer b.Process.Kill()
	time.Sleep(100 * time.Millisecond)

	ua := NewUnit(&parser.Unit{Name: "a.service"}, "")
	ua.GetConfig().Service.ExecStart = "/usr/bin/sleep 300"
	ub := NewUnit(&parser.Unit{Name: "b.service"}, "")
	ub.GetConfig().Service.ExecStart = "/usr/bin/sleep 301"

	pa, _ := ua.FindExternalPID()
	pb, _ := ub.FindExternalPID()
	if pa == 0 || pb == 0 {
		t.Fatalf("expected both to find their sleep, got %d %d", pa, pb)
	}
	if pa == pb {
		t.Fatalf("basename-only match: both units claimed pid %d", pa)
	}
}

func TestEffectiveStateFailedStaysFailed(t *testing.T) {
	u := NewUnit(&parser.Unit{Name: "x.service"}, "")
	u.GetConfig().Service.ExecStart = "/usr/bin/sleep 302"
	// Mark failed; even with a live external sleep 302 nearby it must stay failed.
	u.MarkFailed("boom")
	snap := u.Snapshot()
	if snap.State != StateFailed {
		t.Fatalf("setup: want failed, got %v", snap.State)
	}
	// Start a matching external to prove we don't resurrect.
	cmd := exec.Command("sleep", "302")
	if err := cmd.Start(); err != nil {
		t.Skip("sleep not available")
	}
	defer cmd.Process.Kill()
	time.Sleep(100 * time.Millisecond)
	st, _ := u.EffectiveState()
	if st != StateFailed {
		t.Fatalf("failed must stay failed with external running, got %v", st)
	}
	// Inactive with no external stays inactive.
	u2 := NewUnit(&parser.Unit{Name: "y.service"}, "")
	u2.GetConfig().Service.ExecStart = "/nonexistent-binary-xyz-123 1"
	st2, _ := u2.EffectiveState()
	if st2 != StateInactive {
		t.Fatalf("want inactive, got %v", st2)
	}
}

func TestConfigSetGetConcurrent(t *testing.T) {
	u := NewUnit(&parser.Unit{Name: "x.service"}, "")
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			u.SetConfig(&parser.Unit{Name: "x.service"})
			_ = u.GetConfig()
		}
		close(done)
	}()
	for i := 0; i < 200; i++ {
		_ = u.GetConfig()
		// Exercise a typical reader path that used to race.
		_ = u.Description()
	}
	<-done
}
