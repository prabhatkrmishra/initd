package main

import "testing"

func TestParseArgs(t *testing.T) {
	opts, err := parseArgs([]string{"-u", "ssh", "-n", "50"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(opts.units) != 1 || opts.units[0] != "ssh" || opts.lines != 50 {
		t.Fatalf("opts = %+v", opts)
	}

	opts, err = parseArgs([]string{"--unit=foo.service", "-n20", "--no-pager", "-o", "json"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(opts.units) != 1 || opts.lines != 20 {
		t.Fatalf("opts = %+v", opts)
	}

	opts, err = parseArgs([]string{"-f", "--no-tail", "-u", "ssh"})
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	if !opts.follow || !opts.noTail {
		t.Fatalf("follow opts = %+v", opts)
	}
	if _, err := parseArgs([]string{"--bogus"}); err == nil {
		t.Fatal("unknown option should fail")
	}
	if _, err := parseArgs([]string{"-u"}); err == nil {
		t.Fatal("missing unit value should fail")
	}
	if _, err := parseArgs([]string{"-n", "-5"}); err == nil {
		t.Fatal("negative lines should fail")
	}
	opts, err = parseArgs([]string{"-n", "+5"})
	if err != nil || opts.lines != 5 || !opts.linesPlus {
		t.Fatalf("+N lines = %+v, %v", opts, err)
	}
	opts, err = parseArgs([]string{"-S", "yesterday", "-U", "today", "-p", "err", "-g", "boom", "-b", "-1"})
	if err != nil {
		t.Fatalf("filters: %v", err)
	}
	if opts.since != "yesterday" || opts.until != "today" || !opts.prioritySet || opts.priority != 3 || opts.grep != "boom" || !opts.bootSet || opts.boot != "-1" {
		t.Fatalf("filter opts = %+v", opts)
	}
	if _, err := parseArgs([]string{"--case-sensitive"}); err == nil {
		t.Fatal("--case-sensitive without --grep should fail")
	}
	if _, err := parseArgs([]string{"-M", "foo"}); err == nil {
		t.Fatal("machine scoping should fail loudly")
	}
	// The exact flags from the field report: truncation no-ops must parse.
	for _, flag := range []string{"-l", "--full"} {
		opts, err := parseArgs([]string{"--user", "-u", "hermes-gateway", flag, "--since", "2 min ago"})
		if err != nil {
			t.Fatalf("parse %s: %v", flag, err)
		}
		if len(opts.units) != 1 || opts.units[0] != "hermes-gateway" || opts.since != "2 min ago" {
			t.Fatalf("%s opts = %+v", flag, opts)
		}
	}
}
