package dbus

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"

	dbus "github.com/godbus/dbus/v5"
)

// transientProperty is the a(sv) element shape the manager receives. A typed
// struct (not []any) matters: it is what gives the array elements their
// 8-byte alignment on the wire. The alias keeps the slice identical to the
// anonymous struct in the manager's method signature.
type transientProperty = struct {
	Name  string
	Value dbus.Variant
}

type transientAuxUnit = struct {
	Name  string
	Value []transientProperty
}

// logindSessionScopeCall builds the StartTransientUnit call systemd-logind 255
// makes when it opens a session scope. The captured payload matters as a
// whole: PIDFDs is an "ah" array of descriptors, and it sits in the middle of
// the property array where a decoder failure truncates everything after it.
func logindSessionScopeCall(t *testing.T, pidfd int) *dbus.Message {
	t.Helper()
	props := []transientProperty{
		{Name: "Slice", Value: dbus.MakeVariant("user-1000.slice")},
		{Name: "Description", Value: dbus.MakeVariant("Session c31 of User pkm")},
		{Name: "Wants", Value: dbus.MakeVariant([]string{"user-runtime-dir@1000.service"})},
		{Name: "Wants", Value: dbus.MakeVariant([]string{"user@1000.service"})},
		{Name: "After", Value: dbus.MakeVariant([]string{"systemd-logind.service"})},
		{Name: "After", Value: dbus.MakeVariant([]string{"user-runtime-dir@1000.service"})},
		{Name: "After", Value: dbus.MakeVariant([]string{"user@1000.service"})},
		{Name: "RequiresMountsFor", Value: dbus.MakeVariant("/home/pkm")},
		{Name: "SendSIGHUP", Value: dbus.MakeVariant(true)},
		{Name: "PIDFDs", Value: dbus.MakeVariant([]dbus.UnixFD{dbus.UnixFD(pidfd)})},
		{Name: "OOMPolicy", Value: dbus.MakeVariant("continue")},
		{Name: "TasksMax", Value: dbus.MakeVariant(uint64(18446744073709551615))},
	}
	return &dbus.Message{
		Type: dbus.TypeMethodCall,
		Headers: map[dbus.HeaderField]dbus.Variant{
			dbus.FieldPath:        dbus.MakeVariant(dbus.ObjectPath("/org/freedesktop/systemd1")),
			dbus.FieldDestination: dbus.MakeVariant("org.freedesktop.systemd1"),
			dbus.FieldInterface:   dbus.MakeVariant("org.freedesktop.systemd1.Manager"),
			dbus.FieldMember:      dbus.MakeVariant("StartTransientUnit"),
			dbus.FieldSignature:   dbus.MakeVariant(dbus.ParseSignatureMust("ssa(sv)a(sa(sv))")),
		},
		Body: []any{"session-c31.scope", "fail", props, []transientAuxUnit{}},
	}
}

// TestStartTransientUnitDecodesLogindPayload is the regression test for the
// "Invalid type / number of args" reply that made every pam_systemd session
// fail. godbus v5.2.2 could not decode an array of UNIX fds: it appended
// UnixFD values to a []UnixFDIndex, reflect panicked with a plain string, and
// Decode's recover mistook that for "not an error" and returned a truncated
// body. The manager then saw two arguments instead of four.
func TestStartTransientUnitDecodesLogindPayload(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	msg := logindSessionScopeCall(t, int(f.Fd()))
	var buf bytes.Buffer
	fds, err := msg.EncodeToWithFDs(&buf, binary.LittleEndian)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := dbus.DecodeMessageWithFDs(bytes.NewReader(buf.Bytes()), fds)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Body) != 4 {
		t.Fatalf("decoded %d arguments, want 4 (message body was truncated)", len(decoded.Body))
	}

	var (
		name  string
		mode  string
		props []transientProperty
		aux   []transientAuxUnit
	)
	if err := dbus.Store(decoded.Body, &name, &mode, &props, &aux); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if name != "session-c31.scope" || mode != "fail" {
		t.Fatalf("name=%q mode=%q", name, mode)
	}

	spec, derr := parseTransientScopeProperties(props)
	if derr != nil {
		t.Fatalf("parse: %v", derr)
	}
	if spec.Slice != "user-1000.slice" {
		t.Fatalf("Slice = %q", spec.Slice)
	}
	if spec.Description != "Session c31 of User pkm" {
		t.Fatalf("Description = %q", spec.Description)
	}
	if len(spec.PIDFDs) != 1 || spec.PIDFDs[0] != int(f.Fd()) {
		t.Fatalf("PIDFDs = %v, want [%d]", spec.PIDFDs, f.Fd())
	}
	if len(spec.PIDs) != 0 {
		t.Fatalf("PIDs = %v, logind sent none", spec.PIDs)
	}
}
