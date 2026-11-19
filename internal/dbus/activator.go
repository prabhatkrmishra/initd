package dbus

import (
	"context"
	"fmt"
	"strings"
	"sync"

	dbus "github.com/godbus/dbus/v5"
)

// D-Bus service activation, driven the way systemd sees it.
//
// When a client asks the bus daemon to start a name that has no owner, the
// daemon reads the .service file for the name. Files installed by systemd
// carry `SystemdService=<unit>`, so the daemon asks initd (the owner of
// org.freedesktop.systemd1) to activate that unit by emitting the
// org.freedesktop.systemd1.Activator.ActivationRequest signal, then waits up
// to service_start_timeout for the requested bus name to appear.
//
// initd never implemented the handler, so dbus-daemon waited the full
// timeout and failed the activation, and every pam_systemd login stalled
// 25 seconds before the session channel opened. This file subscribes to the
// signal and starts the unit, which is all dbus-daemon asks of us: it polls
// the bus name, not our answer.

const (
	activatorInterface = "org.freedesktop.systemd1.Activator"
	activatorMember    = "ActivationRequest"
	activatorSignal    = activatorInterface + "." + activatorMember

	dbusDaemonBus  = "org.freedesktop.DBus"
	dbusDaemonPath = "/org/freedesktop/DBus"
)

// activationHandler starts units requested over the Activator signal.
// start is injected so tests never touch a real bus or supervisor; logf may
// be nil, in which case messages are dropped.
//
// The signal arrives on the bus I/O goroutine. Handling is asynchronous on
// purpose: StartUnit blocks until the job finishes, and the bus name must
// stay answerable while activation runs, so the caller gets an empty answer
// immediately and dbus-daemon's own timeout is the backstop. The handler is
// also the dedupe point: one activation of a unit at a time, because the
// login1 case produced a storm of duplicate requests.
type activationHandler struct {
	start func(unit string) error
	logf  func(format string, args ...interface{})

	mu       sync.Mutex
	inflight map[string]struct{}
}

func newActivationHandler(start func(unit string) error, logf func(format string, args ...interface{})) *activationHandler {
	return &activationHandler{
		start:    start,
		logf:     logf,
		inflight: map[string]struct{}{},
	}
}

// parseActivationUnit extracts the unit to start from an ActivationRequest
// signal body. dbus-daemon sends a single string argument: the value of
// SystemdService= from the .service file (e.g. "dbus-org.freedesktop.login1.service").
func parseActivationUnit(body []interface{}) (string, error) {
	if len(body) == 0 {
		return "", fmt.Errorf("activation request without arguments")
	}
	name, ok := body[0].(string)
	if !ok {
		return "", fmt.Errorf("activation request with non-string first argument (%T)", body[0])
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("activation request with an empty unit name")
	}
	return name, nil
}

// handle acknowledges an ActivationRequest and starts the unit in the
// background. It returns immediately; progress is reported through the log.
func (h *activationHandler) handle(sig *dbus.Signal) {
	if sig == nil {
		h.log("activation request ignored: nil signal")
		return
	}
	name, err := parseActivationUnit(sig.Body)
	if err != nil {
		h.log("activation request ignored: %v", err)
		return
	}

	h.mu.Lock()
	if _, busy := h.inflight[name]; busy {
		h.mu.Unlock()
		h.log("activation request for %s ignored: already in flight", name)
		return
	}
	h.inflight[name] = struct{}{}
	h.mu.Unlock()

	go func() {
		defer func() {
			h.mu.Lock()
			delete(h.inflight, name)
			h.mu.Unlock()
		}()
		// A panic inside a supervisor call must not take initd down with it.
		defer func() {
			if r := recover(); r != nil {
				h.log("activation of %s panicked: %v", name, r)
			}
		}()
		if err := h.start(name); err != nil {
			h.log("activation of %s failed: %v", name, err)
		}
	}()
}

// IsInFlight reports whether a start for unit is still running. Exposed for
// tests and diagnostics.
func (h *activationHandler) IsInFlight(unit string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.inflight[unit]
	return ok
}

func (h *activationHandler) log(format string, args ...interface{}) {
	if h.logf != nil {
		h.logf(format, args...)
	}
}

// isActivationSignal reports whether sig is the one this daemon acts on.
// Matched on the bus, not just on the member name, so a different sender
// cannot push unit names at us.
func isActivationSignal(sig *dbus.Signal) bool {
	return sig != nil &&
		sig.Name == activatorSignal &&
		sig.Sender == dbusDaemonBus &&
		string(sig.Path) == dbusDaemonPath
}

// watchActivatorSignals subscribes conn to the Activator signal and starts
// requested units in the background until ctx is done.
//
// The connection is a fresh, dedicated one in practice: godbus dispatches
// signals on a single per-connection channel, so a signal that this watcher
// does not consume would never reach another goroutine. Everything read off
// the channel here is checked against sender and interface before it is
// acted on.
//
// A failure to add the match rule is returned to the caller; the manager
// logs it loudly and keeps serving, because a D-Bus connection with no
// activation path is still worth having.
func watchActivatorSignals(ctx context.Context, conn *dbus.Conn, start func(unit string) error, logf func(format string, args ...interface{})) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := conn.AddMatchSignal(
		dbus.WithMatchSender(dbusDaemonBus),
		dbus.WithMatchObjectPath(dbus.ObjectPath(dbusDaemonPath)),
		dbus.WithMatchInterface(activatorInterface),
		dbus.WithMatchMember(activatorMember),
	); err != nil {
		return fmt.Errorf("add activator match: %w", err)
	}

	handler := newActivationHandler(start, logf)
	ch := make(chan *dbus.Signal, 16)
	conn.Signal(ch)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case sig, ok := <-ch:
				if !ok {
					return
				}
				if !isActivationSignal(sig) {
					continue
				}
				handler.handle(sig)
			}
		}
	}()
	return nil
}
