package dbus

import (
	"errors"
	"fmt"

	dbus "github.com/godbus/dbus/v5"

	"initd/internal/supervisor"
)

// D-Bus StartTransientUnit, scoped down to what logind actually needs:
// .scope transient units carrying a set of PIDs. Service and socket transient
// units are refused with NotSupported; StartTransientUnit exists here so that
// pam_systemd can open session scopes, which is the only transient unit the
// login path creates.

// transientScopeSpec is the parsed subset of the a(sv) properties argument.
// Anything else is ignored, as systemd ignores properties it does not know.
type transientScopeSpec struct {
	Description     string
	PIDs            []int
	Slice           string
	Delegate        bool
	RemainAfterExit bool
}

// parseTransientScopeProperties reads the recognized properties out of an
// a(sv) array. Unknown properties are dropped; known ones with the wrong
// type are an InvalidArgs error.
func parseTransientScopeProperties(props []struct {
	Name  string
	Value dbus.Variant
}) (transientScopeSpec, *dbus.Error) {
	var spec transientScopeSpec
	for _, p := range props {
		switch p.Name {
		case "Description":
			s, ok := variantString(p.Value)
			if !ok {
				return spec, invalidArgs("Description must be a string")
			}
			spec.Description = s
		case "PIDs":
			pids, ok := variantInts(p.Value)
			if !ok {
				return spec, invalidArgs("PIDs must be an array of integers")
			}
			spec.PIDs = pids
		case "Slice":
			s, ok := variantString(p.Value)
			if !ok {
				return spec, invalidArgs("Slice must be a string")
			}
			spec.Slice = s
		case "Delegate":
			b, ok := variantBool(p.Value)
			if !ok {
				return spec, invalidArgs("Delegate must be a boolean")
			}
			spec.Delegate = b
		case "RemainAfterExit":
			b, ok := variantBool(p.Value)
			if !ok {
				return spec, invalidArgs("RemainAfterExit must be a boolean")
			}
			spec.RemainAfterExit = b
		default:
			// Unknown property: ignore, like systemd.
		}
	}
	for _, pid := range spec.PIDs {
		if pid <= 0 {
			return spec, invalidArgs("PIDs must be positive process IDs")
		}
	}
	return spec, nil
}

func variantString(v dbus.Variant) (string, bool) {
	s, ok := v.Value().(string)
	return s, ok
}

func variantBool(v dbus.Variant) (bool, bool) {
	b, ok := v.Value().(bool)
	return b, ok
}

func variantInts(v dbus.Variant) ([]int, bool) {
	switch x := v.Value().(type) {
	case []uint32:
		out := make([]int, len(x))
		for i, n := range x {
			out[i] = int(n)
		}
		return out, true
	case []uint64:
		out := make([]int, len(x))
		for i, n := range x {
			out[i] = int(n)
		}
		return out, true
	case []int:
		return x, true
	case []int64:
		out := make([]int, len(x))
		for i, n := range x {
			out[i] = int(n)
		}
		return out, true
	}
	return nil, false
}

func invalidArgs(msg string) *dbus.Error {
	return &dbus.Error{Name: "org.freedesktop.DBus.Error.InvalidArgs", Body: []interface{}{msg}}
}

// scopeUnitNameError maps the supervisor's name validation onto D-Bus error
// names: a non-.scope name is NotSupported, anything else is InvalidArgs.
func scopeUnitNameError(name string) *dbus.Error {
	err := supervisor.ValidateTransientScopeName(name)
	if err == nil {
		return nil
	}
	if errors.Is(err, supervisor.ErrNotScopeUnit) {
		return &dbus.Error{Name: "org.freedesktop.systemd1.NotSupported", Body: []interface{}{fmt.Sprintf("Only .scope transient units are supported, got %q", name)}}
	}
	return invalidArgs(err.Error())
}

// StartTransientUnit implements org.freedesktop.systemd1.Manager.StartTransientUnit
// for scopes. The unit is created and started inside a job; the job path is
// returned immediately, like every other mutating call.
func (m *systemd1Manager) StartTransientUnit(name string, mode string, properties []struct {
	Name  string
	Value dbus.Variant
}, aux []struct {
	Name  string
	Value []struct {
		Name  string
		Value dbus.Variant
	}
}) (dbus.ObjectPath, *dbus.Error) {
	_ = mode
	_ = aux // Auxiliary units are not supported; logind does not use them.
	if derr := scopeUnitNameError(name); derr != nil {
		return "", derr
	}
	spec, derr := parseTransientScopeProperties(properties)
	if derr != nil {
		return "", derr
	}
	mgr := m.primarySafe()
	// A name that resolves to something already is refused synchronously,
	// like a missing unit on the other side; the creation itself happens in
	// the job.
	if jobUnitKnown(mgr, name) {
		return "", &dbus.Error{Name: "org.freedesktop.systemd1.UnitExists", Body: []interface{}{fmt.Sprintf("Unit %s already exists.", name)}}
	}
	return queueUnitJob(mgr, m.jobs, m.logf, name, "start", func(unit string) error {
		return mgr.StartTransientScope(unit, supervisor.TransientScope{
			Description:     spec.Description,
			PIDs:            spec.PIDs,
			Slice:           spec.Slice,
			Delegate:        spec.Delegate,
			RemainAfterExit: spec.RemainAfterExit,
		})
	})
}
