package main

import (
	"bytes"
	"strings"
	"testing"

	"initd/internal/logging"
)

func TestParseNewFilterFlags(t *testing.T) {
	opts, err := parseArgs([]string{"-T", "spam", "--invocation", "abc", "-I", "-W", "-i", "/tmp/x.journal"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if opts.excludeIdentifier != "spam" || opts.invocation != "abc" || !opts.latestInvocation || !opts.noHostname || opts.file != "/tmp/x.journal" {
		t.Fatalf("opts = %+v", opts)
	}
	opts, err = parseArgs([]string{"--exclude-identifier=noise", "--list-invocations", "--list-namespaces", "--synchronize-on-exit=false"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if opts.excludeIdentifier != "noise" || !opts.listInvocations || !opts.listNamespaces {
		t.Fatalf("opts = %+v", opts)
	}
}

func TestShortDeltaFormat(t *testing.T) {
	a := logging.StoredEntry{Seq: 1, RealtimeUsec: 1000000, MonotonicUsec: 2000000, BootID: "b", Identifier: "x", PID: 1, Message: "first"}
	b := logging.StoredEntry{Seq: 2, RealtimeUsec: 1500000, MonotonicUsec: 2500000, BootID: "b", Identifier: "x", PID: 1, Message: "second"}
	got := formatEntries([]logging.StoredEntry{a, b}, "short-delta", false, false, "")
	if len(got) != 2 || !strings.Contains(got[1], "0.500000") {
		t.Fatalf("delta = %v", got)
	}
	if strings.Contains(got[1], "*") {
		t.Fatalf("same-boot delta should be reliable: %v", got)
	}
	c := b
	c.BootID = "other"
	got = formatEntries([]logging.StoredEntry{a, c}, "short-delta", false, false, "")
	if !strings.Contains(got[1], "*") {
		t.Fatalf("cross-boot delta should be starred: %v", got)
	}
}

func TestStreamMatchesFormat(t *testing.T) {
	entries := []logging.StoredEntry{
		{Seq: 1, RealtimeUsec: 1000000, MonotonicUsec: 1000000, BootID: "b", Unit: "a.service", Identifier: "a", PID: 7, Message: "one"},
		{Seq: 2, RealtimeUsec: 2000000, MonotonicUsec: 2000000, BootID: "b", Unit: "a.service", Identifier: "a", PID: 7, Message: "two"},
	}
	for _, mode := range []string{"short", "short-delta", "json", "json-sse", "json-seq", "export", "cat"} {
		want := strings.Join(formatEntries(entries, mode, false, false, ""), "\n") + "\n"
		var buf bytes.Buffer
		streamEntries(&buf, entries, mode, false, false, "")
		if buf.String() != want {
			t.Fatalf("mode %s: stream=%q want=%q", mode, buf.String(), want)
		}
	}
}

func TestResolvePagerPrecedence(t *testing.T) {
	t.Setenv("SYSTEMD_PAGER", "more -s")
	t.Setenv("PAGER", "less -R")
	name, args, ok := resolvePager()
	if !ok || name != "more" || len(args) != 1 || args[0] != "-s" {
		t.Fatalf("got %q %v %v", name, args, ok)
	}
	t.Setenv("SYSTEMD_PAGER", "")
	name, _, ok = resolvePager()
	if ok {
		t.Fatalf("empty SYSTEMD_PAGER should disable, got %q", name)
	}
}
