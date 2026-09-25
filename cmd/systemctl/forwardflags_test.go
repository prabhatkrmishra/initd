package main

import (
	"reflect"
	"testing"
)

func TestCommandIndex(t *testing.T) {
	cases := []struct {
		in   []string
		want int
	}{
		{[]string{"status", "foo"}, 0},
		{[]string{"--no-pager", "status", "foo"}, 1},
		{[]string{"-n", "20", "status", "foo"}, 2},
		{[]string{"--property", "status", "foo"}, 2}, // status is the value
		{[]string{"--socket", "/tmp/x.sock", "status"}, 2},
		{[]string{"--no-pager"}, -1},
		{[]string{}, -1},
	}
	for _, c := range cases {
		if got := commandIndex(c.in); got != c.want {
			t.Errorf("commandIndex(%v)=%d want %d", c.in, got, c.want)
		}
	}
}

func TestSplitForwardedFlags(t *testing.T) {
	args := []string{"--no-pager", "-q", "status", "foo"}
	fwd, kept := splitForwardedFlags(args, 2)
	if !reflect.DeepEqual(fwd, []string{"--no-pager", "-q"}) {
		t.Fatalf("forwarded=%v", fwd)
	}
	if !reflect.DeepEqual(kept, []string{"status", "foo"}) {
		t.Fatalf("kept=%v", kept)
	}
	// Value flag keeps its value with it.
	args = []string{"-n", "20", "status", "foo"}
	fwd, kept = splitForwardedFlags(args, 2)
	if !reflect.DeepEqual(fwd, []string{"-n", "20"}) {
		t.Fatalf("forwarded=%v", fwd)
	}
	// Unknown flags stay so flag.Parse still rejects them.
	args = []string{"--bogus", "status", "foo"}
	fwd, kept = splitForwardedFlags(args, 1)
	if len(fwd) != 0 || !reflect.DeepEqual(kept, args) {
		t.Fatalf("fwd=%v kept=%v", fwd, kept)
	}
}

func TestSplitStatusLogOpts(t *testing.T) {
	rest, lines := splitStatusLogOpts([]string{"-n", "5", "foo"})
	if lines != 5 || !reflect.DeepEqual(rest, []string{"foo"}) {
		t.Fatalf("got %v %d", rest, lines)
	}
	_, lines = splitStatusLogOpts([]string{"foo"})
	if lines != 10 {
		t.Fatalf("default=%d want 10", lines)
	}
	_, lines = splitStatusLogOpts([]string{"--lines=all", "foo"})
	if lines != -1 {
		t.Fatalf("all=%d want -1", lines)
	}
}

// TestIsNotFoundMessageKeepsRealFailuresReal: the daemon says "unit X not
// found" for an unknown unit, but a known unit can fail with errno prose that
// also contains "not found"/"no such". Classifying those as not-found made a
// broken EnvironmentFile look like a missing unit.
func TestIsNotFoundMessage(t *testing.T) {
	yes := []string{
		"unit zz-nope.service not found",
		"Unit zz-nope.service not found",
		"unit foo.service not loaded",
		"unit bar.service could not be found",
	}
	no := []string{
		"open /tmp/zzenvfile: no such file or directory",
		"fork/exec /usr/bin/id: no such file or directory",
		"required unit missing.service not found",
		"unit zz-user.service is masked",
		"exit status 1",
	}
	for _, msg := range yes {
		if !isNotFoundMessage(msg) {
			t.Errorf("isNotFoundMessage(%q) = false, want true", msg)
		}
	}
	for _, msg := range no {
		if isNotFoundMessage(msg) {
			t.Errorf("isNotFoundMessage(%q) = true, want false", msg)
		}
	}
}
