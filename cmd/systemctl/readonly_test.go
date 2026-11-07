package main

import "testing"

// Read verbs may travel over the unprivileged read socket; everything else
// must stay on the control socket, where the daemon authorizes mutations.
func TestReadOnlyCommand(t *testing.T) {
	for _, cmd := range []string{
		"status", "show", "cat", "is-active", "is-enabled", "is-failed",
		"is-system-running", "list-units", "list-unit-files", "help",
	} {
		if !readOnlyCommand(cmd) {
			t.Errorf("%s should use the read-only socket", cmd)
		}
	}
	for _, cmd := range []string{
		"start", "stop", "restart", "reload", "reenable", "enable",
		"disable", "preset", "preset-all", "try-restart", "kill",
		"reset-failed", "mask", "unmask", "daemon-reload", "daemon-reexec",
		"reboot", "poweroff", "halt", "log", "frobnicate", "",
	} {
		if readOnlyCommand(cmd) {
			t.Errorf("%s must stay on the control socket", cmd)
		}
	}
}
