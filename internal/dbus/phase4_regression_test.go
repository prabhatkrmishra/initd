package dbus

import (
	"testing"
	"time"
)

func TestDbusEscapeUnderscoreRoundTrip(t *testing.T) {
	for _, name := range []string{"foo.service", "foo_bar.service", "a:b/c d.service", "openclaw-gateway.service"} {
		esc := string(unitObjectPath(name))
		back, ok := unitNameFromPath(esc)
		if !ok || back != name {
			t.Fatalf("round-trip %q -> %q -> %q (ok=%v)", name, esc, back, ok)
		}
	}
	// '_' must be escaped (systemd behavior), not left literal.
	if got := dbusEscape("foo_bar"); got != "foo_5fbar" {
		t.Fatalf("dbusEscape(foo_bar) = %q, want foo_5fbar", got)
	}
}

func TestShellSplitEmptyArgs(t *testing.T) {
	got := shellSplitExecStart(`/bin/foo "" bar`)
	if len(got) != 3 || got[0] != "/bin/foo" || got[1] != "" || got[2] != "bar" {
		t.Fatalf("middle empty dropped: %#v", got)
	}
	got = shellSplitExecStart(`/bin/foo bar ""`)
	if len(got) != 3 || got[2] != "" {
		t.Fatalf("trailing empty dropped: %#v", got)
	}
	got = shellSplitExecStart(`/bin/foo "a b" c`)
	if len(got) != 3 || got[1] != "a b" {
		t.Fatalf("quoted space split: %#v", got)
	}
}

func TestStartUnitMissingErrors(t *testing.T) {
	mgr := newTestManager(t)
	m := newManager(mgr)
	if _, derr := m.StartUnit("does-not-exist.service", "fail"); derr == nil {
		t.Fatalf("StartUnit on missing unit should error, not succeed")
	} else if derr.Name != "org.freedesktop.systemd1.NoSuchUnit" {
		t.Fatalf("want NoSuchUnit, got %v", derr.Name)
	}
}

func TestListUnitsFilteredHonorsFilter(t *testing.T) {
	mgr := newTestManager(t)
	searchDir := mgr.SearchPaths[0]
	writeUnitForDBus(t, searchDir, "run.service", "[Service]\nExecStart=/bin/sleep 30\n")
	writeUnitForDBus(t, searchDir, "idle.service", "[Service]\nType=oneshot\nExecStart=/bin/true\n")
	if err := mgr.LoadUnits(); err != nil {
		t.Fatal(err)
	}
	m := newManager(mgr)
	if _, derr := m.StartUnit("run.service", "fail"); derr != nil {
		t.Fatalf("start run: %v", derr)
	}
	if _, derr := m.StartUnit("idle.service", "fail"); derr != nil {
		t.Fatalf("start idle: %v", derr)
	}
	// Let oneshot finish.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if u, err := mgr.FindUnit("idle.service"); err == nil {
			if snap := u.Snapshot(); snap.State != "activating" {
				break
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, derr := m.ListUnitsFiltered([]string{"active"})
	if derr != nil {
		t.Fatal(derr)
	}
	for _, u := range got {
		if u.ActiveState != "active" {
			t.Fatalf("filtered list contains %q=%q", u.Name, u.ActiveState)
		}
	}
	found := false
	for _, u := range got {
		if u.Name == "run.service" {
			found = true
		}
		if u.Name == "idle.service" {
			t.Fatalf("inactive idle.service must not pass active filter")
		}
	}
	if !found {
		t.Fatalf("active run.service missing from filtered list")
	}
	_ = mgr.StopUnit("run.service")
}
