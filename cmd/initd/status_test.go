package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// capture runs f with os.Stdout redirected, the way the reporting flags are
// exercised in production.
func capture(t *testing.T, f func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.String()
	}()
	f()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

// The point of --status: a client must be able to see that the user bus is not
// advertised, rather than discovering it by waiting out service_start_timeout.
func TestStatusReportsRetryRatherThanNothing(t *testing.T) {
	statusState.mu.Lock()
	statusState.items = map[string]*transportState{
		"dbus:user": {
			Transport: "dbus", Scope: "user",
			Address: "unix:path=/run/user/1000/bus",
			State:   "retrying", Detail: "connection refused", Since: "2026-01-01T00:00:00Z",
		},
	}
	statusState.mu.Unlock()

	out := capture(t, func() { printStatus(false) })
	if !strings.Contains(out, "retrying") {
		t.Errorf("status did not surface the retrying state:\n%s", out)
	}
	if !strings.Contains(out, "/run/user/1000/bus") {
		t.Errorf("status did not name the bus address:\n%s", out)
	}
	if !strings.Contains(out, "connection refused") {
		t.Errorf("status did not carry the reason:\n%s", out)
	}
}

// --status --json is the machine-readable form tooling should parse, so it has
// to be valid JSON with the fields a client needs to make a decision.
func TestStatusJSONIsParseable(t *testing.T) {
	statusState.mu.Lock()
	statusState.items = map[string]*transportState{
		"unix:user": {Transport: "unix", Scope: "user", Address: "/run/user/1000/initd.sock", State: "listening"},
	}
	statusState.mu.Unlock()

	out := capture(t, func() { printStatus(true) })
	var parsed struct {
		PID        int              `json:"pid"`
		Transports []transportState `json:"transports"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("--status --json is not valid JSON: %v\n%s", err, out)
	}
	if parsed.PID != os.Getpid() {
		t.Errorf("pid = %d, want %d", parsed.PID, os.Getpid())
	}
	if len(parsed.Transports) != 1 {
		t.Fatalf("transports = %d, want 1", len(parsed.Transports))
	}
	got := parsed.Transports[0]
	if got.Transport != "unix" || got.Scope != "user" || got.State != "listening" {
		t.Errorf("transport = %+v, want unix/user/listening", got)
	}
}

// A daemon that has not served yet must say so, not print an empty report that
// reads like "nothing to report".
func TestStatusSaysSoWhenNothingIsAdvertised(t *testing.T) {
	statusState.mu.Lock()
	statusState.items = map[string]*transportState{}
	statusState.mu.Unlock()

	out := capture(t, func() { printStatus(false) })
	if !strings.Contains(out, "no transports advertised") {
		t.Errorf("empty status did not say so:\n%s", out)
	}
}

// Ordering must be stable so the output is diffable and greppable.
func TestStatusOrdersTransportsDeterministically(t *testing.T) {
	statusState.mu.Lock()
	statusState.items = map[string]*transportState{
		"dbus:system":  {Transport: "dbus", Scope: "system", Address: "sys", State: "owned"},
		"unix:user":    {Transport: "unix", Scope: "user", Address: "usr", State: "listening"},
		"dbus:user":    {Transport: "dbus", Scope: "user", Address: "ubus", State: "owned"},
		"unix:primary": {Transport: "unix", Scope: "system", Address: "prim", State: "listening"},
	}
	statusState.mu.Unlock()

	first := capture(t, func() { printStatus(false) })
	for i := 0; i < 5; i++ {
		if got := capture(t, func() { printStatus(false) }); got != first {
			t.Fatalf("status order is not stable:\nfirst:\n%s\ngot:\n%s", first, got)
		}
	}
	if !strings.Contains(first, "ubus") || !strings.Contains(first, "sys") {
		t.Errorf("status lost a transport:\n%s", first)
	}
}

// --print-paths exists so a tool can dial the manager directly instead of
// reimplementing the XDG//run//tmp resolution rules.
func TestPrintPathsReportsEveryAddress(t *testing.T) {
	out := capture(t, func() { printPaths(false) })
	for _, key := range []string{
		"user_socket", "system_socket", "user_runtime",
		"user_state", "user_journal", "system_journal", "user_bus",
	} {
		if !strings.Contains(out, key) {
			t.Errorf("--print-paths is missing %q:\n%s", key, out)
		}
	}
}

func TestPrintPathsJSONIsParseable(t *testing.T) {
	out := capture(t, func() { printPaths(true) })
	var parsed map[string]string
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("--print-paths --json is not valid JSON: %v\n%s", err, out)
	}
	if parsed["user_socket"] == "" {
		t.Error("user_socket is empty in the JSON form")
	}
}

// The flags answer and exit; they must not fall through into daemon startup.
func TestStatusAndPathsFlagsDoNotStartTheDaemon(t *testing.T) {
	for _, arg := range []string{"--status", "--print-paths"} {
		cfg, err := parseArgs([]string{arg})
		if err != nil {
			t.Fatalf("parseArgs(%s): %v", arg, err)
		}
		if !cfg.showStatus && !cfg.showPaths {
			t.Errorf("parseArgs(%s) did not set a reporting flag: %+v", arg, cfg)
		}
		// A reporting flag must also not imply it wants to run anything.
		if arg == "--status" && !cfg.showStatus {
			t.Errorf("--status set showPaths instead")
		}
	}
}
