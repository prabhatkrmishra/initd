package supervisor

import (
	"os"
	"path/filepath"
	"testing"
)

func socketModeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

func TestSocketBindGetsDefaultMode0666(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := filepath.Join(dir, "default.sock")
	writeUnit(t, dir, "zzdefault.socket", "[Socket]\nListenStream="+sockPath+"\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	if err := m.StartUnit("zzdefault.socket"); err != nil {
		t.Fatalf("StartUnit: %v", err)
	}
	if got := socketModeOf(t, sockPath); got != 0o666 {
		t.Fatalf("socket mode = %o, want 666", got)
	}
	_ = m.StopUnit("zzdefault.socket")
}

func TestSocketBindSocketModeOverride(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := filepath.Join(dir, "override.sock")
	writeUnit(t, dir, "zzoverride.socket", "[Socket]\nListenStream="+sockPath+"\nSocketMode=0600\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	if err := m.StartUnit("zzoverride.socket"); err != nil {
		t.Fatalf("StartUnit: %v", err)
	}
	if got := socketModeOf(t, sockPath); got != 0o600 {
		t.Fatalf("socket mode = %o, want 600", got)
	}
	_ = m.StopUnit("zzoverride.socket")
}

func TestSocketBindSocketModeInvalidFallsBackTo0666(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := filepath.Join(dir, "invalid.sock")
	writeUnit(t, dir, "zzinvalid.socket", "[Socket]\nListenStream="+sockPath+"\nSocketMode=zz\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	if err := m.StartUnit("zzinvalid.socket"); err != nil {
		t.Fatalf("StartUnit: %v", err)
	}
	if got := socketModeOf(t, sockPath); got != 0o666 {
		t.Fatalf("socket mode = %o, want 666 for an unparsable SocketMode", got)
	}
	_ = m.StopUnit("zzinvalid.socket")
}

func TestDatagramSocketGetsDefaultMode0666(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := filepath.Join(dir, "dgram.sock")
	writeUnit(t, dir, "zzdgram.socket", "[Socket]\nListenDatagram="+sockPath+"\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	if err := m.StartUnit("zzdgram.socket"); err != nil {
		t.Fatalf("StartUnit: %v", err)
	}
	if got := socketModeOf(t, sockPath); got != 0o666 {
		t.Fatalf("datagram socket mode = %o, want 666", got)
	}
	_ = m.StopUnit("zzdgram.socket")
}

func TestDatagramSocketModeOverride(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := filepath.Join(dir, "dgram-override.sock")
	writeUnit(t, dir, "zzdvra.socket", "[Socket]\nListenDatagram="+sockPath+"\nSocketMode=0640\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	if err := m.StartUnit("zzdvra.socket"); err != nil {
		t.Fatalf("StartUnit: %v", err)
	}
	if got := socketModeOf(t, sockPath); got != 0o640 {
		t.Fatalf("datagram socket mode = %o, want 640", got)
	}
	_ = m.StopUnit("zzdvra.socket")
}

func TestShowUnitRefusesSocketUnits(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := filepath.Join(dir, "refused.sock")
	writeUnit(t, dir, "zzrefused.socket", "[Socket]\nListenStream="+sockPath+"\n")
	if err := m.LoadUnits(); err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	if _, err := m.ShowUnit("zzrefused.socket"); err == nil {
		t.Fatal("ShowUnit(foo.socket) must report not-found")
	}
	if _, err := m.CatUnit("zzrefused.socket"); err == nil {
		t.Fatal("CatUnit(foo.socket) must report not-found")
	}
	if _, err := m.ShowSocketUnit("zzrefused.socket"); err != nil {
		t.Fatalf("ShowSocketUnit(foo.socket): %v", err)
	}
	if err := m.StartUnit("zzrefused.socket"); err != nil {
		t.Fatalf("StartUnit(socket): %v", err)
	}
	state, err := m.SocketActiveState("zzrefused.socket")
	if err != nil || state != "active" {
		t.Fatalf("SocketActiveState = %q, %v", state, err)
	}
	// A FindUnit attempt must not resurrect a fake service unit either.
	if u, err := m.FindUnit("zzrefused.socket"); err == nil {
		t.Fatalf("FindUnit(foo.socket) returned fake service unit %v", u.GetConfig().Name)
	}
	_ = m.StopUnit("zzrefused.socket")
}
