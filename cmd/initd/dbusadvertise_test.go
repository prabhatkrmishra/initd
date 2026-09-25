package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	gdbus "github.com/godbus/dbus/v5"
	"initd/internal/dbus"
)

// The failure this replaces: the daemon tried the session bus five times over
// about two seconds and then gave up for good. A session bus that appears
// later - profile.d/initd.sh forks dbus-daemon, a session starts without one -
// left the daemon permanently invisible to every D-Bus client, and those clients
// then stalled on service activation instead.
func TestAdvertiseKeepsTryingUntilTheBusAppears(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var attempts int32
	registered := make(chan struct{}, 1)
	// The bus appears only after more attempts than the old five-try loop
	// allowed. That number is the point of the test: a bus that is merely slow
	// is the common case, and the old code gave up on exactly that.
	const appearAfter = 9
	register := func(context.Context) (*gdbus.Conn, error) {
		if atomic.AddInt32(&attempts, 1) < appearAfter {
			return nil, errors.New("dial unix /run/user/1000/bus: connect: connection refused")
		}
		select {
		case registered <- struct{}{}:
		default:
		}
		return nil, errHeldForTest
	}

	go advertise(ctx, "test bus", register)

	select {
	case <-registered:
	case <-time.After(30 * time.Second):
		t.Fatalf("advertise gave up after %d attempts instead of retrying until the bus appeared",
			atomic.LoadInt32(&attempts))
	}
	if n := atomic.LoadInt32(&attempts); n < appearAfter {
		t.Errorf("attempts = %d, want at least %d", n, appearAfter)
	}
}

// errHeldForTest is returned by the fake registrar to mean "acquired"; advertise
// then polls liveness, and a nil conn must read as not-alive so it re-acquires
// instead of spinning on a connection it does not have.
var errHeldForTest = errors.New("held")

// A registrar that fails forever must not spin: backoff has to grow, or a box
// with no bus at all burns a core retrying every 100ms for the daemon's life.
func TestAdvertiseBacksOffWhenTheBusNeverAppears(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var attempts int32
	register := func(context.Context) (*gdbus.Conn, error) {
		atomic.AddInt32(&attempts, 1)
		return nil, errors.New("no bus")
	}
	go advertise(ctx, "absent bus", register)

	time.Sleep(1500 * time.Millisecond)
	got := atomic.LoadInt32(&attempts)
	// With 100ms doubling backoff, 1.5s is roughly 5-7 attempts. A spin would
	// be in the hundreds.
	if got > 20 {
		t.Errorf("attempts in 1.5s = %d; backoff is not growing", got)
	}
	if got < 3 {
		t.Errorf("attempts in 1.5s = %d, want the retry to keep trying", got)
	}
}

// Cancelling the context is the only exit: the daemon must not leave a
// goroutine advertising a name after it has begun shutting down.
func TestAdvertiseStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var attempts int32
	done := make(chan struct{})
	register := func(context.Context) (*gdbus.Conn, error) {
		atomic.AddInt32(&attempts, 1)
		return nil, errors.New("no bus")
	}
	go func() {
		advertise(ctx, "test bus", register)
		close(done)
	}()

	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("advertise did not return after its context was cancelled")
	}
	before := atomic.LoadInt32(&attempts)
	time.Sleep(300 * time.Millisecond)
	if after := atomic.LoadInt32(&attempts); after != before {
		t.Errorf("attempts kept growing after cancel (%d -> %d)", before, after)
	}
}

// jitter must stay near the requested delay: too tight and concurrent daemons
// synchronise, too wide and the cap is meaningless.
func TestJitterStaysNearTheRequest(t *testing.T) {
	for _, d := range []time.Duration{100 * time.Millisecond, time.Second, 30 * time.Second} {
		for i := 0; i < 200; i++ {
			got := jitter(d)
			lo, hi := d-d/10-time.Millisecond, d+d/10+time.Millisecond
			if got < lo || got > hi {
				t.Fatalf("jitter(%v) = %v, outside [%v,%v]", d, got, lo, hi)
			}
		}
	}
	if got := jitter(time.Microsecond); got < time.Millisecond {
		t.Errorf("jitter(1us) = %v, want the 1ms floor", got)
	}
}

// advertise calls dbus.ConnAlive on whatever the registrar returned, including
// nil, so the nil case must be an answer rather than a panic.
func TestConnAliveNilIsNotAlive(t *testing.T) {
	if dbus.ConnAlive(nil) {
		t.Error("a nil connection reported alive")
	}
}
