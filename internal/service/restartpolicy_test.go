package service

import (
	"testing"
	"time"

	"initd/internal/parser"
)

func policyUnit(svc parser.ServiceSection, interval, burst string) *Unit {
	return &Unit{Config: &parser.Unit{
		Name:                  "flap.service",
		Service:               svc,
		StartLimitIntervalSec: interval,
		StartLimitBurst:       burst,
	}}
}

func TestStartLimitDefaults(t *testing.T) {
	u := policyUnit(parser.ServiceSection{}, "", "")
	interval, burst := u.StartLimit()
	if interval != 10*time.Second || burst != 5 {
		t.Fatalf("defaults = %v/%d, want 10s/5", interval, burst)
	}
}

func TestStartLimitCustomAndDisabled(t *testing.T) {
	u := policyUnit(parser.ServiceSection{}, "30", "3")
	interval, burst := u.StartLimit()
	if interval != 30*time.Second || burst != 3 {
		t.Fatalf("custom = %v/%d, want 30s/3", interval, burst)
	}
	u = policyUnit(parser.ServiceSection{}, "0", "5")
	interval, _ = u.StartLimit()
	if interval != 0 {
		t.Fatalf("interval 0 should disable, got %v", interval)
	}
}

func TestRestartDelayGrowth(t *testing.T) {
	u := policyUnit(parser.ServiceSection{
		RestartSec: "2s", RestartSteps: "3", RestartMaxDelaySec: "20s",
	}, "", "")
	// Three growth steps then hold: 2, 4, 8, 16, 16, ... (cap 20s unreached).
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 16 * time.Second, 16 * time.Second}
	for i, w := range want {
		if got := u.RestartDelay(i + 1); got != w {
			t.Fatalf("attempt %d delay = %v, want %v", i+1, got, w)
		}
	}
	// Without steps the delay still caps at the max.
	u = policyUnit(parser.ServiceSection{
		RestartSec: "2s", RestartMaxDelaySec: "5s",
	}, "", "")
	if got := u.RestartDelay(10); got != 5*time.Second {
		t.Fatalf("uncapped growth = %v, want 5s", got)
	}
}

func TestRestartDelayFixed(t *testing.T) {
	u := policyUnit(parser.ServiceSection{RestartSec: "5"}, "", "")
	for _, n := range []int{1, 2, 5} {
		if got := u.RestartDelay(n); got != 5*time.Second {
			t.Fatalf("attempt %d delay = %v, want 5s", n, got)
		}
	}
	plain := policyUnit(parser.ServiceSection{}, "", "")
	if got := plain.RestartDelay(3); got != 0 {
		t.Fatalf("unset RestartSec should mean no delay, got %v", got)
	}
}

func TestShouldRestartMatrix(t *testing.T) {
	u := policyUnit(parser.ServiceSection{SuccessExitStatus: "1"}, "", "")
	cases := []struct {
		mode string
		code int
		want bool
	}{
		{"always", 0, true},
		{"always", 1, true},
		{"on-failure", 1, false}, // listed success: not a failure
		{"on-failure", 2, true},
		{"on-failure", 0, false},
		{"on-success", 0, true},
		{"on-success", 1, true}, // listed success
		{"on-success", 2, false},
		{"on-abnormal", 143, true}, // 128+SIGTERM
		{"on-abnormal", 1, false},
		{"on-abnormal", 0, false},
		{"on-watchdog", 1, false},
		{"no", 1, false},
	}
	for _, c := range cases {
		if got := u.ShouldRestart(c.mode, c.code); got != c.want {
			t.Errorf("ShouldRestart(%q, %d) = %v, want %v", c.mode, c.code, got, c.want)
		}
	}
}
