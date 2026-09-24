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
