package dbus

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	dbus "github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

func scopeProperties(t *testing.T, pid int) []struct {
	Name  string
	Value dbus.Variant
} {
	t.Helper()
	return []struct {
		Name  string
		Value dbus.Variant
	}{
		{Name: "Description", Value: dbus.MakeVariant("dbus test scope")},
		{Name: "PIDs", Value: dbus.MakeVariant([]uint32{uint32(pid)})},
		{Name: "Slice", Value: dbus.MakeVariant("-.slice")},
		{Name: "Delegate", Value: dbus.MakeVariant(true)},
		{Name: "RemainAfterExit", Value: dbus.MakeVariant(false)},
	}
}

func spawnScopeProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start /bin/sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

func TestParseTransientScopeProperties(t *testing.T) {
	props := scopeProperties(t, os.Getpid())
	spec, derr := parseTransientScopeProperties(props)
	if derr != nil {
		t.Fatalf("parse: %v", derr)
	}
	if spec.Description != "dbus test scope" || len(spec.PIDs) != 1 || spec.PIDs[0] != os.Getpid() {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.Slice != "-.slice" || !spec.Delegate || spec.RemainAfterExit {
		t.Fatalf("spec = %+v", spec)
	}
}

func TestParseTransientScopePropertiesIgnoresUnknownKeys(t *testing.T) {
	props := scopeProperties(t, os.Getpid())
	props = append(props, struct {
		Name  string
		Value dbus.Variant
	}{Name: "SomeFutureProperty", Value: dbus.MakeVariant("whatever")})
	if _, derr := parseTransientScopeProperties(props); derr != nil {
		t.Fatalf("unknown property must be ignored, got %v", derr)
	}
}

func TestParseTransientScopePropertiesRejectsBadTypes(t *testing.T) {
	bad := []struct {
		name string
		prop struct {
			Name  string
			Value dbus.Variant
		}
	}{
		{"Description as int", struct {
			Name  string
			Value dbus.Variant
		}{Name: "Description", Value: dbus.MakeVariant(42)}},
		{"PIDs as string", struct {
			Name  string
			Value dbus.Variant
		}{Name: "PIDs", Value: dbus.MakeVariant("123")}},
		{"Delegate as int", struct {
			Name  string
			Value dbus.Variant
		}{Name: "Delegate", Value: dbus.MakeVariant(1)}},
		{"RemainAfterExit as string", struct {
			Name  string
			Value dbus.Variant
		}{Name: "RemainAfterExit", Value: dbus.MakeVariant("yes")}},
	}
	for _, tc := range bad {
		if _, derr := parseTransientScopeProperties([]struct {
			Name  string
			Value dbus.Variant
		}{tc.prop}); derr == nil {
			t.Errorf("%s: expected an error", tc.name)
		} else if derr.Name != "org.freedesktop.DBus.Error.InvalidArgs" {
			t.Errorf("%s: error name = %q, want InvalidArgs", tc.name, derr.Name)
		}
	}
}

func TestParseTransientScopePropertiesRejectsNonPositivePIDs(t *testing.T) {
	props := []struct {
		Name  string
		Value dbus.Variant
	}{
		{Name: "PIDs", Value: dbus.MakeVariant([]uint32{0, 1234})},
	}
	if _, derr := parseTransientScopeProperties(props); derr == nil {
		t.Fatal("PIDs containing 0 must be rejected")
	}
}

func TestParseTransientScopePropertiesPIDFDs(t *testing.T) {
	props := []struct {
		Name  string
		Value dbus.Variant
	}{
		{Name: "PIDFDs", Value: dbus.MakeVariant([]dbus.UnixFD{3, 4})},
	}
	spec, derr := parseTransientScopeProperties(props)
	if derr != nil {
		t.Fatalf("parse: %v", derr)
	}
	if len(spec.PIDFDs) != 2 || spec.PIDFDs[0] != 3 || spec.PIDFDs[1] != 4 {
		t.Fatalf("PIDFDs = %v, want [3 4]", spec.PIDFDs)
	}
	if len(spec.PIDs) != 0 {
		t.Fatalf("PIDs = %v, want none", spec.PIDs)
	}
}

func TestParseTransientScopePropertiesRejectsWireOnlyPIDFDs(t *testing.T) {
	// UnixFDIndex values are message indices, not descriptors. Treating them
	// as descriptors would close unrelated file descriptors.
	props := []struct {
		Name  string
		Value dbus.Variant
	}{
		{Name: "PIDFDs", Value: dbus.MakeVariant([]dbus.UnixFDIndex{0})},
	}
	if _, derr := parseTransientScopeProperties(props); derr == nil || derr.Name != "org.freedesktop.DBus.Error.InvalidArgs" {
		t.Fatalf("wire-only PIDFDs: %v, want InvalidArgs", derr)
	}
}

func TestPIDFromPIDFD(t *testing.T) {
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
			t.Skipf("pidfd_open unsupported: %v", err)
		}
		t.Fatalf("pidfd_open: %v", err)
	}
	pid, err := pidFromPIDFD(fd)
	if err != nil {
		t.Fatalf("pidFromPIDFD: %v", err)
	}
	if pid != os.Getpid() {
		t.Fatalf("pid = %d, want %d", pid, os.Getpid())
	}
	closeUnixFDs([]int{fd})
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("descriptor %d still open after closeUnixFDs: %v", fd, err)
	}
}

func TestScopeUnitNameError(t *testing.T) {
	for _, name := range []string{"foo.scope", "session-2.scope", "user@1000.scope", "a-b_c.d:1.scope"} {
		if derr := scopeUnitNameError(name); derr != nil {
			t.Errorf("%q should be valid: %v", name, derr)
		}
	}
	if derr := scopeUnitNameError(""); derr == nil {
		t.Error("empty name must be rejected")
	}
	if derr := scopeUnitNameError("foo.service"); derr == nil || derr.Name != "org.freedesktop.systemd1.NotSupported" {
		t.Fatalf("foo.service: %v, want NotSupported", derr)
	}
	if derr := scopeUnitNameError("foo.scope/evil"); derr == nil {
		t.Error("path separators must be rejected")
	}
	if derr := scopeUnitNameError(".scope"); derr == nil {
		t.Error("name with empty base must be rejected")
	}
	if derr := scopeUnitNameError("...scope"); derr == nil {
		t.Error("name with dots-only base must be rejected")
	}
	if derr := scopeUnitNameError(".hidden.scope"); derr == nil {
		t.Error("leading dot must be rejected")
	}
	if derr := scopeUnitNameError(strings.Repeat("a", 300) + ".scope"); derr == nil {
		t.Error("over-long names must be rejected")
	}
}

func TestStartTransientUnitReturnsJobAndCreatesScope(t *testing.T) {
	mgr := newTestManager(t)
	m := newManager(mgr)
	p := spawnScopeProcess(t)

	path, derr := m.StartTransientUnit("zzdbus-start.scope", "replace", scopeProperties(t, p.Process.Pid), nil)
	if derr != nil {
		t.Fatalf("StartTransientUnit: %v", derr)
	}
	if !strings.HasPrefix(string(path), "/org/freedesktop/systemd1/job/") {
		t.Fatalf("path = %q, want a job path", path)
	}
	waitForJobs(t, m.jobs, 0)

	unit, err := mgr.FindUnit("zzdbus-start.scope")
	if err != nil {
		t.Fatalf("scope unit missing after the job finished: %v", err)
	}
	if got := unit.Snapshot().State; got != "active" {
		t.Fatalf("scope state = %q, want active", got)
	}
	_ = mgr.StopUnit("zzdbus-start.scope")
}

func TestStartTransientUnitDuplicateIsUnitExists(t *testing.T) {
	mgr := newTestManager(t)
	m := newManager(mgr)
	p := spawnScopeProcess(t)

	if _, derr := m.StartTransientUnit("zzdbus-dup.scope", "replace", scopeProperties(t, p.Process.Pid), nil); derr != nil {
		t.Fatalf("first StartTransientUnit: %v", derr)
	}
	waitForJobs(t, m.jobs, 0)
	_, derr := m.StartTransientUnit("zzdbus-dup.scope", "replace", scopeProperties(t, p.Process.Pid), nil)
	if derr == nil {
		t.Fatal("second creation with the same name must fail")
	}
	if derr.Name != "org.freedesktop.systemd1.UnitExists" {
		t.Fatalf("error name = %q, want UnitExists", derr.Name)
	}
	_ = mgr.StopUnit("zzdbus-dup.scope")
}

func TestStartTransientUnitWrongKindIsNotSupported(t *testing.T) {
	mgr := newTestManager(t)
	m := newManager(mgr)
	if _, derr := m.StartTransientUnit("zzdbus-wrong.service", "replace", nil, nil); derr == nil {
		t.Fatal("non-.scope creation must fail")
	} else if derr.Name != "org.freedesktop.systemd1.NotSupported" {
		t.Fatalf("error name = %q, want NotSupported", derr.Name)
	}
	if jobs := m.jobs.list(); len(jobs) != 0 {
		t.Fatalf("jobs = %v, want none after a refused creation", jobs)
	}
}

func TestStartTransientUnitMissingPIDsStillActivates(t *testing.T) {
	mgr := newTestManager(t)
	m := newManager(mgr)
	props := []struct {
		Name  string
		Value dbus.Variant
	}{
		{Name: "Description", Value: dbus.MakeVariant("empty scope")},
		{Name: "RemainAfterExit", Value: dbus.MakeVariant(true)},
	}
	path, derr := m.StartTransientUnit("zzdbus-empty.scope", "replace", props, nil)
	if derr != nil {
		t.Fatalf("StartTransientUnit: %v", derr)
	}
	if !strings.HasPrefix(string(path), "/org/freedesktop/systemd1/job/") {
		t.Fatalf("path = %q", path)
	}
	waitForJobs(t, m.jobs, 0)
	unit, err := mgr.FindUnit("zzdbus-empty.scope")
	if err != nil {
		t.Fatalf("scope missing: %v", err)
	}
	if got := unit.Snapshot().State; got != "active" {
		t.Fatalf("state = %q, want active (RemainAfterExit)", got)
	}
	_ = mgr.StopUnit("zzdbus-empty.scope")
}
