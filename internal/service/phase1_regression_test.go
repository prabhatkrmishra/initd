package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"initd/internal/parser"
)

func TestParseSystemdDurationTable(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"5", 5 * time.Second},
		{"5s", 5 * time.Second},
		{"500ms", 500 * time.Millisecond},
		{"5min", 5 * time.Minute},
		{"5m", 5 * time.Minute},
		{"1h", time.Hour},
		{"1d", 24 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
		{"1M", 30 * 24 * time.Hour},
		{"1y", 365 * 24 * time.Hour},
		{"1.5h", 90 * time.Minute},
		{"0", 0},
		{"infinity", 0},
	}
	for _, c := range cases {
		if got := parseSystemdDuration(c.in, 10*time.Second); got != c.want {
			t.Fatalf("parseSystemdDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	if got := parseSystemdDuration("bogus-unit", 10*time.Second); got != 10*time.Second {
		t.Fatalf("unknown suffix should fall back to default, got %v", got)
	}
}

func TestEnvironmentFileExportAndQuotes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	content := "# comment\nFOO=bar\n export BAZ=qux\nSINGLE='a b'\nDOUBLE=\"c d\"\nEMPTY=\n; semicolon comment\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	u := NewUnit(&parser.Unit{Name: "x.service"}, "")
	envMap := map[string]string{}
	if err := u.loadEnvironmentFile(path, envMap); err != nil {
		t.Fatalf("loadEnvironmentFile: %v", err)
	}
	if envMap["FOO"] != "bar" || envMap["BAZ"] != "qux" {
		t.Fatalf("export handling wrong: %v", envMap)
	}
	if envMap["SINGLE"] != "a b" || envMap["DOUBLE"] != "c d" {
		t.Fatalf("quote handling wrong: %v", envMap)
	}
	if _, ok := envMap["export FOO"]; ok {
		t.Fatalf("export prefix leaked into key: %v", envMap)
	}
}

func TestRuntimeDirectoryTrailingSlash(t *testing.T) {
	u := NewUnit(&parser.Unit{Name: "x.service"}, "")
	for _, n := range []string{"foo/", "./bar", "a//b", "trailing-slash-test-xyz/"} {
		base := t.TempDir()
		if _, err := u.ensureNamedDirectories(base, []string{n}, "0755"); err != nil {
			t.Fatalf("harmless form %q should be accepted, got %v", n, err)
		}
	}
}

func TestProcessAliveDeadPid(t *testing.T) {
	if processAlive(0) || processAlive(-5) {
		t.Fatalf("non-positive pid must be dead")
	}
	// Max pid unlikely to exist; should be dead, not alive.
	if processAlive(1 << 22) {
		t.Fatalf("absurd pid should be dead")
	}
	if !processAlive(os.Getpid()) {
		t.Fatalf("self should be alive")
	}
}
