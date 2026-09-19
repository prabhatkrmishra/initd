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

	if _, err := parseArgs([]string{"-f"}); err == nil {
		t.Fatal("follow mode should fail")
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
}
