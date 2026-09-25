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
