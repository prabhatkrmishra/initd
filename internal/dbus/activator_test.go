package dbus

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	dbus "github.com/godbus/dbus/v5"
)

func activationSignal(body ...interface{}) *dbus.Signal {
	return &dbus.Signal{
		Sender: "org.freedesktop.DBus",
		Path:   "/org/freedesktop/DBus",
		Name:   "org.freedesktop.systemd1.Activator.ActivationRequest",
		Body:   body,
	}
}

func TestParseActivationUnit(t *testing.T) {
	name, err := parseActivationUnit([]interface{}{"dbus-org.freedesktop.login1.service"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if name != "dbus-org.freedesktop.login1.service" {
		t.Fatalf("name = %q", name)
	}
	for _, body := range [][]interface{}{
		{},
		{""},
		{"   "},
		{42},
		{nil},
	} {
		if _, err := parseActivationUnit(body); err == nil {
			t.Errorf("parseActivationUnit(%v) = nil error, want one", body)
		}
	}
}

func TestIsActivationSignalMatchesOnlyDbusDaemon(t *testing.T) {
	if !isActivationSignal(activationSignal("x.service")) {
		t.Fatal("expected the daemon's own signal to match")
	}
	cases := []*dbus.Signal{
		nil,
		{Sender: "org.freedesktop.DBus", Name: "org.freedesktop.systemd1.Manager.Reloading"},
		{Sender: "org.freedesktop.DBus", Path: "/org/freedesktop/DBus", Name: "org.freedesktop.systemd1.Manager.Reloading"},
		{Sender: "org.freedesktop.DBus", Path: "/other", Name: "org.freedesktop.systemd1.Activator.ActivationRequest"},
		{Sender: ":1.99", Path: "/org/freedesktop/DBus", Name: "org.freedesktop.systemd1.Activator.ActivationRequest"},
		{Sender: "org.freedesktop.DBus", Path: "/org/freedesktop/DBus", Name: "org.freedesktop.systemd1.Activator.ActivationRequestExtra"},
	}
	for i, sig := range cases {
		if isActivationSignal(sig) {
			t.Errorf("case %d: unexpected match for %+v", i, sig)
		}
	}
}

func TestActivationHandlerStartsUnit(t *testing.T) {
	started := make(chan string, 1)
	handler := newActivationHandler(func(unit string) error {
		started <- unit
		return nil
	}, nil)

	handler.handle(activationSignal("dbus-org.freedesktop.login1.service"))

	select {
	case unit := <-started:
		if unit != "dbus-org.freedesktop.login1.service" {
			t.Fatalf("started %q", unit)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("start was not called")
	}
}

func TestActivationHandlerReturnsWithoutBlockingStart(t *testing.T) {
	release := make(chan struct{})
	handler := newActivationHandler(func(string) error {
		<-release
		return nil
	}, nil)
	defer close(release)

	done := make(chan struct{})
	go func() {
		handler.handle(activationSignal("slow.service"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("handle blocked on a slow start")
	}
}

func TestActivationHandlerDeduplicatesInFlightStart(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	release := make(chan struct{})
	handler := newActivationHandler(func(unit string) error {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release
		return nil
	}, nil)

	handler.handle(activationSignal("dbus-org.freedesktop.login1.service"))
	// Wait until the first start is actually running.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		running := calls
		mu.Unlock()
		if running == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	handler.handle(activationSignal("dbus-org.freedesktop.login1.service"))
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	count := calls
	mu.Unlock()
	if count != 1 {
		t.Fatalf("in-flight start was not deduplicated: calls = %d", count)
	}

	close(release)
	// Once the first start finishes, a new request must trigger a fresh start.
	waitInflight(t, handler, "dbus-org.freedesktop.login1.service", false)
	handler.handle(activationSignal("dbus-org.freedesktop.login1.service"))
	deadline = time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		count = calls
		mu.Unlock()
		if count == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if count != 2 {
		t.Fatalf("after completion a new request did not start again: calls = %d", count)
	}
}

func waitInflight(t *testing.T, handler *activationHandler, unit string, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if handler.IsInFlight(unit) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("inflight(%s) never became %v", unit, want)
}

func TestActivationHandlerLogsErrorAndAllowsRetry(t *testing.T) {
	var logs []string
	var mu sync.Mutex
	calls := 0
	handler := newActivationHandler(func(string) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return fmt.Errorf("unit not found")
	}, func(format string, args ...interface{}) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	})

	handler.handle(activationSignal("missing.service"))
	waitInflight(t, handler, "missing.service", false)

	mu.Lock()
	n := calls
	joined := strings.Join(logs, "\n")
	mu.Unlock()
	if n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
	if !strings.Contains(joined, "missing.service") || !strings.Contains(joined, "unit not found") {
		t.Fatalf("error was not logged: %q", joined)
	}

	// A failed start clears the in-flight marker, so a retry is attempted.
	handler.handle(activationSignal("missing.service"))
	waitInflight(t, handler, "missing.service", false)
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestActivationHandlerBadSignalsAreIgnored(t *testing.T) {
	called := make(chan string, 4)
	handler := newActivationHandler(func(unit string) error {
		called <- unit
		return nil
	}, nil)
	for _, sig := range []*dbus.Signal{
		nil,
		activationSignal(),
		activationSignal(42),
		activationSignal(""),
		activationSignal("   "),
	} {
		handler.handle(sig)
	}
	select {
	case unit := <-called:
		t.Fatalf("bad signal started unit %q", unit)
	case <-time.After(200 * time.Millisecond):
	}
}
