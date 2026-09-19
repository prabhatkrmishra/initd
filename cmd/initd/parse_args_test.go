package main

import "testing"

func TestParseArgsDefaults(t *testing.T) {
	cfg, err := parseArgs([]string{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.socketPath != "/run/initd.sock" || !cfg.initMode || cfg.daemonize {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestParseArgsDaemonize(t *testing.T) {
	cfg, err := parseArgs([]string{"--init", "--daemonize"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cfg.daemonize || !cfg.initMode || cfg.pidFile == "" {
		t.Fatalf("daemonize = %+v, want pid default set", cfg)
	}

	cfg, err = parseArgs([]string{"--daemonize", "--pid-file", "/tmp/x.pid", "--log-file=/tmp/x.log"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.pidFile != "/tmp/x.pid" || cfg.logFile != "/tmp/x.log" {
		t.Fatalf("paths = %+v", cfg)
	}

	if _, err := parseArgs([]string{"--pid-file"}); err == nil {
		t.Fatal("missing pid path should fail")
	}
	if _, err := parseArgs([]string{"--bogus"}); err == nil {
		t.Fatal("unknown flag should fail")
	}
}

func TestParseArgsSocketOnly(t *testing.T) {
	cfg, err := parseArgs([]string{"--socket", "/tmp/a.sock"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.initMode || cfg.socketPath != "/tmp/a.sock" {
		t.Fatalf("socket-only = %+v", cfg)
	}
}
