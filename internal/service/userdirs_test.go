package service

import (
	"os"
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
	if run != userpaths.UserRuntimeDir() || state != userpaths.UserStateDir() {
		t.Fatalf("run/state = %q/%q", run, state)
	}
	if cache != userpaths.UserCacheDir() || conf != userpaths.UserConfigHome() {
		t.Fatalf("cache/conf = %q/%q", cache, conf)
	}
	if logs == "" || logs == "/var/log" {
		t.Fatalf("logs must be user-scoped off-root, got %q", logs)
	}
}
