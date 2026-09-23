package main

import (
	"os/user"
	"testing"
)

func TestParseOptionsStrict(t *testing.T) {
	opts, pos, err := parseOptions([]string{"--no-legend", "--value", "-p", "Linger", "show-user", "root"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !opts.noLegend || !opts.value || opts.property != "Linger" {
		t.Fatalf("opts = %+v", opts)
	}
	if len(pos) != 2 || pos[0] != "show-user" {
		t.Fatalf("positional = %v", pos)
	}
	if _, _, err := parseOptions([]string{"--nolegend", "list-users"}); err == nil {
		t.Fatal("typo flag should fail")
	}
	if _, _, err := parseOptions([]string{"--bogus", "list-users"}); err == nil {
		t.Fatal("unknown flag should fail")
	}
	opts, _, err = parseOptions([]string{"-P", "Linger"})
	if err != nil || !opts.value || opts.property != "Linger" {
		t.Fatalf("-P = %+v, %v", opts, err)
	}
	if _, _, err := parseOptions([]string{"--json=bogus"}); err == nil {
		t.Fatal("bad --json should fail")
	}
}

func TestLingerRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	u, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	name := u.Username
	if isLinger(name) {
		t.Fatal("linger should start empty in temp state dir")
	}
	if code := handleLinger("enable-linger", []string{name}); code != 0 {
		t.Fatalf("enable = %d", code)
	}
	if !isLinger(name) {
		t.Fatal("linger file missing after enable")
	}
	found := false
	for _, n := range lingerUsers() {
		if n == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("lingerUsers = %v", lingerUsers())
	}
	if code := handleLinger("disable-linger", []string{name}); code != 0 {
		t.Fatalf("disable = %d", code)
	}
	if isLinger(name) {
		t.Fatal("linger file present after disable")
	}
	if code := handleLinger("enable-linger", []string{"no-such-user-xyz"}); code == 0 {
		t.Fatal("enable for unknown user should fail")
	}
}
