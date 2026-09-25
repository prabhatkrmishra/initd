package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"initd/internal/parser"
)

// systemScopeUnit is a unit held by a system manager, which is the only scope
// where PassEnvironment= decides anything. The scope belongs to the manager, not
// to the process: one daemon runs both, so this is how a test looks at system
// rules without being root.
func systemScopeUnit(t *testing.T) *Unit {
	t.Helper()
	u := NewUnit(&parser.Unit{Name: "p.service"}, "")
	u.SetUserMode(false)
	return u
}

// A unit that names a manager variable is opting out of initd's blanket
// inheritance, because that is what upstream does with it: the system manager
// passes exactly those names and nothing else, and a name the manager does not
// have is not an error, just absent.
func TestPassEnvironmentFiltersTheInheritedEnvironment(t *testing.T) {
	t.Setenv("ZZPASS_NAMED", "wanted")
	t.Setenv("ZZPASS_UNNAMED", "leaked")

	u := systemScopeUnit(t)
	u.GetConfig().Service.PassEnvironment = []string{"ZZPASS_NAMED", "ZZPASS_NOTINTHEMANAGER"}
	envMap, _, err := u.buildEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if envMap["ZZPASS_NAMED"] != "wanted" {
		t.Errorf("passed variable = %q, want the manager's value", envMap["ZZPASS_NAMED"])
	}
	if _, ok := envMap["ZZPASS_UNNAMED"]; ok {
		t.Error("an unnamed manager variable reached a unit that asked for the other one")
	}
	if _, ok := envMap["ZZPASS_NOTINTHEMANAGER"]; ok {
		t.Error("a name the manager does not have was invented out of nothing")
	}
}

// Environment= and EnvironmentFile= are the unit's own declarations and survive
// the filter; UnsetEnvironment= still has the last word over a passed name.
func TestPassEnvironmentLeavesUnitDeclarationsAlone(t *testing.T) {
	t.Setenv("ZZPASS_NAMED", "from-manager")
	t.Setenv("ZZPASS_DROPPED", "from-manager")

	u := systemScopeUnit(t)
	u.GetConfig().Service.PassEnvironment = []string{"ZZPASS_NAMED", "ZZPASS_DROPPED"}
	u.GetConfig().Service.Environment = []string{"ZZPASS_OWN=1"}
	u.GetConfig().Service.UnsetEnvironment = []string{"ZZPASS_DROPPED"}
	envMap, _, err := u.buildEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if envMap["ZZPASS_NAMED"] != "from-manager" || envMap["ZZPASS_OWN"] != "1" {
		t.Errorf("passed=%q own=%q, want both kept", envMap["ZZPASS_NAMED"], envMap["ZZPASS_OWN"])
	}
	if _, ok := envMap["ZZPASS_DROPPED"]; ok {
		t.Error("UnsetEnvironment= lost to a name on the PassEnvironment= list")
	}
}

// A user manager gives every unit its whole environment whether the unit names
// anything or not, so initd's inheritance has to stay. Nothing here depends on
// the uid of the process, which is how a root daemon's own user units keep that
// behaviour too.
func TestPassEnvironmentIsInertInUserScope(t *testing.T) {
	t.Setenv("ZZPASS_NAMED", "wanted")
	t.Setenv("ZZPASS_UNNAMED", "inherited")

	u := NewUnit(&parser.Unit{Name: "p.service"}, "")
	u.SetUserMode(true)
	u.GetConfig().Service.PassEnvironment = []string{"ZZPASS_NAMED"}
	envMap, _, err := u.buildEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if envMap["ZZPASS_UNNAMED"] != "inherited" {
		t.Error("a user-scope unit stopped inheriting the manager environment")
	}
}

// With nothing named, the scope that does filter still inherits, which is the
// deviation every unit written against initd so far relies on.
func TestNoPassEnvironmentKeepsInheritanceInSystemScope(t *testing.T) {
	t.Setenv("ZZPASS_UNNAMED", "inherited")

	u := systemScopeUnit(t)
	envMap, _, err := u.buildEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if envMap["ZZPASS_UNNAMED"] != "inherited" {
		t.Error("an empty PassEnvironment= cleared the inheritance it was not asked to touch")
	}
}

// A name given in assignment form is still a name: the directive holds
// variables, and a unit that says FOO=bar means FOO.
func TestPassEnvironmentAcceptsAssignmentFormNames(t *testing.T) {
	t.Setenv("ZZPASS_FORM", "wanted")
	t.Setenv("ZZPASS_OTHER", "leaked")

	u := systemScopeUnit(t)
	u.GetConfig().Service.PassEnvironment = []string{"ZZPASS_FORM=anything"}
	envMap, _, err := u.buildEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if envMap["ZZPASS_FORM"] != "wanted" {
		t.Errorf("ZZPASS_FORM=%q, want it passed by the name before the '='", envMap["ZZPASS_FORM"])
	}
	if _, ok := envMap["ZZPASS_OTHER"]; ok {
		t.Error("the assignment form let every variable through")
	}
}

// A filtered start still gets what the manager computes for itself: losing
// $NOTIFY_SOCKET or $LISTEN_FDS to a filter nobody asked for turns a
// socket-activated notify service into a unit that times out on every start.
// Checked in a real child, which also covers the directive coming through the
// parser rather than being assigned to a config by hand.
func TestFilteredLiveStartKeepsManagerComputedVariables(t *testing.T) {
	t.Setenv("ZZPASS_NAMED", "passed")
	t.Setenv("ZZPASS_UNNAMED", "nothing-should-carry-this")

	out := filepath.Join(t.TempDir(), "child-env")
	body := `echo "${NOTIFY_SOCKET:-missing} ${LISTEN_FDS:-missing} ${ZZPASS_NAMED:-missing} ${ZZPASS_NOTINTHEMANAGER:-absent} ${ZZPASS_UNNAMED:-filtered}" > ` + out
	u := socketActivatedUnit(t, "zzfilter.service",
		"[Service]\nType=notify\nTimeoutStartSec=10\nPassEnvironment=ZZPASS_NAMED ZZPASS_NOTINTHEMANAGER\n"+
			"ExecStart=/bin/sh -c '"+body+"'\n")
	u.SetUserMode(false)
	if _, err := u.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = u.Stop(5 * time.Second) }()

	got := strings.Fields(waitForFile(t, out))
	if len(got) != 5 {
		t.Fatalf("child reported %q, want five fields", got)
	}
	if got[0] == "missing" {
		t.Error("the filter dropped NOTIFY_SOCKET, so a notify unit could never report ready")
	}
	for _, c := range []struct{ name, got, want string }{
		{"LISTEN_FDS", got[1], "1"},
		{"a named manager variable", got[2], "passed"},
		{"a name the manager lacks", got[3], "absent"},
		{"an unnamed manager variable", got[4], "filtered"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// A unit's scope comes from the manager that holds it, and a unit nobody
// claimed falls back to the uid it was built under. Everything about
// PassEnvironment= depends on this answer.
func TestUnitsTakeTheirScopeFromTheirManager(t *testing.T) {
	u := NewUnit(&parser.Unit{Name: "s.service"}, "")
	if got, want := u.inSystemScope(), os.Geteuid() == 0; got != want {
		t.Errorf("unclaimed unit: system scope=%v, want the process uid's answer (%v)", got, want)
	}
	u.SetUserMode(true)
	if u.inSystemScope() {
		t.Error("a unit under a user manager reported system scope")
	}
	u.SetUserMode(false)
	if !u.inSystemScope() {
		t.Error("a unit under a system manager reported user scope")
	}
}

// waitForFile holds the line a unit's ExecStart writes once it is running,
// which is the only proof of what the child was actually given.
func waitForFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil {
			if line := strings.TrimSpace(string(data)); line != "" {
				return line
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was never written: the unit's ExecStart did not run", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
