package service

import (
	"strings"
	"testing"
	"time"

	"initd/internal/parser"
)

func TestHelperTimeout(t *testing.T) {
	u := NewUnit(&parser.Unit{Name: "t.service"}, "")
	start := time.Now()
	_, err := u.runCommandStatus("sleep 5", map[string]string{}, []string{}, commandOptions{}, 200*time.Millisecond)
	el := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timeout error, got %v", err)
	}
	if el > 3*time.Second {
		t.Fatalf("helper did not time out promptly: %v", el)
	}
}

func TestExecStartPostFailurePreserved(t *testing.T) {
	u := NewUnit(&parser.Unit{Name: "t.service"}, "")
	u.GetConfig().Service.ExecStart = "/bin/sleep 10"
	u.GetConfig().Service.ExecStartPost = []string{"/bin/false"}
	if _, err := u.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap := u.Snapshot()
		if snap.State == StateFailed {
			// Give the reaper's SIGTERM exit a chance to clobber.
			time.Sleep(400 * time.Millisecond)
			snap = u.Snapshot()
			if strings.Contains(snap.LastError, "SIGTERM") || strings.Contains(snap.LastError, "143") {
				t.Fatalf("failure reason clobbered: %q", snap.LastError)
			}
			if snap.LastError == "" {
				t.Fatalf("empty LastError after post failure")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("unit did not fail, state=%v err=%q", snap.State, snap.LastError)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
