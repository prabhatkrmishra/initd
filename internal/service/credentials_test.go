package service

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// Templated units carry specifiers in credentials too: User=%i must be
// expanded before the lookup, not looked up literally.
func TestResolveCredentialsExpandsTemplatedUser(t *testing.T) {
	u := newTestUnit(t, "zzspec@1000.service", "[Service]\nUser=%i\nExecStart=/bin/true\n")
	creds, err := u.resolveCredentialsForStart(false)
	if err != nil {
		t.Fatalf("User=%%i must expand to the instance uid: %v", err)
	}
	if !creds.set {
		t.Fatal("expected a credential spec for User=%i")
	}
	if creds.uid != 1000 {
		t.Fatalf("resolved uid = %d, want 1000", creds.uid)
	}

	// Group= is expanded the same way.
	u2 := newTestUnit(t, "zzspec@1000.service", "[Service]\nGroup=%i\nExecStart=/bin/true\n")
	if _, err := u2.resolveCredentialsForStart(false); err != nil {
		t.Fatalf("Group=%%i must expand to the instance gid: %v", err)
	}
}

// A template instance naming an unknown user must fail resolution, not be
// silently started as the daemon.
func TestResolveCredentialsUnknownTemplatedUserFails(t *testing.T) {
	u := newTestUnit(t, "zzspec@zznosuchuser-zz-123456.service", "[Service]\nUser=%i\nExecStart=/bin/true\n")
	_, err := u.resolveCredentialsForStart(false)
	if err == nil {
		t.Fatal("User=%i with an unresolvable instance must fail")
	}
	if !strings.Contains(err.Error(), "lookup user") {
		t.Fatalf("error = %v, want a lookup failure", err)
	}
}

// A unit that runs as a different user than the manager must not inherit the
// manager's home: a root manager spawning uid 1000 with HOME=/root cannot read
// /root, so nothing in the user session ever started.
func TestBuildEnvironmentSetsHomeForOtherUID(t *testing.T) {
	u := newTestUnit(t, "zzhome0.service", "[Service]\nUser=0\nExecStart=/bin/true\n")
	envMap, _, err := u.buildEnvironment()
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	if envMap["HOME"] != "/root" {
		t.Fatalf("HOME = %q, want /root", envMap["HOME"])
	}
	if envMap["USER"] != "root" {
		t.Fatalf("USER = %q, want root", envMap["USER"])
	}
	if envMap["LOGNAME"] != "root" {
		t.Fatalf("LOGNAME = %q, want root", envMap["LOGNAME"])
	}
	if envMap["XDG_RUNTIME_DIR"] != "/run/user/0" {
		t.Fatalf("XDG_RUNTIME_DIR = %q, want /run/user/0", envMap["XDG_RUNTIME_DIR"])
	}
}

// A unit running as the manager's own user keeps the inherited environment.
func TestBuildEnvironmentKeepsOwnerHomeForSameUID(t *testing.T) {
	home := os.Getenv("HOME")
	u := newTestUnit(t, "zzhomesame.service",
		"[Service]\nUser="+strconv.Itoa(os.Getuid())+"\nExecStart=/bin/true\n")
	envMap, _, err := u.buildEnvironment()
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	if envMap["HOME"] != home {
		t.Fatalf("HOME = %q, want the inherited %q", envMap["HOME"], home)
	}
}
