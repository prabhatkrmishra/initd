package service

import (
	"os"
	"path/filepath"
	"testing"

	"initd/internal/userpaths"
)

func TestDirectoryBasesUserScope(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("user-scope roots only apply off-root")
	}
	u := NewUnit(nil, "")
	// NewUnit with nil config would nil-deref in GetConfig users; build a minimal one.
	run, state, cache, logs, conf := u.directoryBases()
	if run != userpaths.UserRuntimeDir() || state != userpaths.UserStateBase() {
		t.Fatalf("run/state = %q/%q", run, state)
	}
	if cache != userpaths.UserCacheBase() || conf != userpaths.UserConfigHome() {
		t.Fatalf("cache/conf = %q/%q", cache, conf)
	}
	// systemd puts a user unit's logs under $XDG_STATE_HOME/log, not in a
	// private subtree.
	if logs != filepath.Join(userpaths.UserStateBase(), "log") {
		t.Fatalf("logs = %q, want %q", logs, filepath.Join(userpaths.UserStateBase(), "log"))
	}
}
