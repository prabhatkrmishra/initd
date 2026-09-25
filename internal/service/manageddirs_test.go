package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// xdgBases points every user-scope directory base at a temp tree so the test
// neither depends on nor pollutes the developer's real ~/.local.
func xdgBases(t *testing.T) (run, state, cache, config string) {
	t.Helper()
	run, state, cache, config = t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", run)
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("XDG_CONFIG_HOME", config)
	return
}

const dirUnitBody = `
[Service]
Type=oneshot
RuntimeDirectory=zzr
StateDirectory=zzs1 zzs2
CacheDirectory=zzc
LogsDirectory=zzl
ConfigurationDirectory=zzg
ExecStart=/bin/true
`

func TestManagedDirectoriesAreExported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("user-scope bases only apply off-root")
	}
	run, state, cache, config := xdgBases(t)
	u := newTestUnit(t, "zzdir.service", dirUnitBody)
	if _, err := u.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	envMap, _, err := u.buildEnvironment()
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	want := map[string]string{
		"RUNTIME_DIRECTORY":       filepath.Join(run, "zzr"),
		"STATE_DIRECTORY":         filepath.Join(state, "zzs1") + ":" + filepath.Join(state, "zzs2"),
		"CACHE_DIRECTORY":         filepath.Join(cache, "zzc"),
		"LOGS_DIRECTORY":          filepath.Join(state, "log", "zzl"),
		"CONFIGURATION_DIRECTORY": filepath.Join(config, "zzg"),
	}
	for key, expected := range want {
		if got := envMap[key]; got != expected {
			t.Errorf("%s = %q, want %q", key, got, expected)
		}
	}
	for _, path := range []string{
		filepath.Join(run, "zzr"),
		filepath.Join(state, "zzs1"),
		filepath.Join(state, "zzs2"),
		filepath.Join(cache, "zzc"),
		filepath.Join(state, "log", "zzl"),
		filepath.Join(config, "zzg"),
	} {
		if st, err := os.Stat(path); err != nil || !st.IsDir() {
			t.Errorf("directory %s was not created: %v", path, err)
		}
	}
}

// TestRuntimeDirectoryRemovedOnStop is the oracle behaviour: a stop deletes
// /run/<name> but leaves state, cache, logs and configuration behind.
func TestRuntimeDirectoryRemovedOnStop(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("user-scope bases only apply off-root")
	}
	run, state, _, _ := xdgBases(t)
	u := newTestUnit(t, "zzstop.service", dirUnitBody)
	if _, err := u.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	runtimeDir := filepath.Join(run, "zzr")
	stateDir := filepath.Join(state, "zzs1")

	if err := u.Stop(2 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(runtimeDir); !os.IsNotExist(err) {
		t.Errorf("runtime directory survived the stop (stat err = %v)", err)
	}
	if st, err := os.Stat(stateDir); err != nil || !st.IsDir() {
		t.Errorf("state directory should survive the stop: %v", err)
	}

	// The next start recreates what the stop removed.
	if _, err := u.Start(); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if st, err := os.Stat(runtimeDir); err != nil || !st.IsDir() {
		t.Errorf("runtime directory was not recreated: %v", err)
	}
}

func TestRuntimeDirectoryPreserveYesKeepsDirs(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("user-scope bases only apply off-root")
	}
	run, _, _, _ := xdgBases(t)
	u := newTestUnit(t, "zzkeep.service", dirUnitBody+"RuntimeDirectoryPreserve=yes\n")
	if _, err := u.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := u.Stop(2 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st, err := os.Stat(filepath.Join(run, "zzr")); err != nil || !st.IsDir() {
		t.Errorf("RuntimeDirectoryPreserve=yes must keep the directory: %v", err)
	}
}

// RuntimeDirectoryPreserve=restart is only about restarts: a plain stop still
// cleans /run, a restart does not.
func TestRuntimeDirectoryPreserveRestart(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("user-scope bases only apply off-root")
	}
	run, _, _, _ := xdgBases(t)
	body := dirUnitBody + "RuntimeDirectoryPreserve=restart\n"
	runtimeDir := filepath.Join(run, "zzr")

	restart := newTestUnit(t, "zzrestart.service", body)
	if _, err := restart.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Write a marker inside so a deletion is observable.
	marker := filepath.Join(runtimeDir, "marker")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	restart.KeepDirectoriesForRestart()
	if err := restart.Stop(2 * time.Second); err != nil {
		t.Fatalf("Stop during restart: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("restart lost the runtime directory contents: %v", err)
	}

	plain := newTestUnit(t, "zzplain.service", body)
	if _, err := plain.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := plain.Stop(2 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(runtimeDir); !os.IsNotExist(err) {
		t.Errorf("a plain stop must remove the runtime directory (stat err = %v)", err)
	}
}
