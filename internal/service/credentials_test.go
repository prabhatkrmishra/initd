package service

import (
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
