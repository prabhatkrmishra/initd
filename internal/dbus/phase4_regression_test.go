package dbus

import (
	"testing"
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
