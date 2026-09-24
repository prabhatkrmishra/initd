package parser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContinuationJoin(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.service")
	content := "[Unit]\nDescription=x\n[Service]\nExecStart=/usr/bin/foo \\\n    --flag1 \\\n    --flag2\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	u, err := ParseUnit(p)
	if err != nil {
		t.Fatalf("ParseUnit: %v", err)
	}
	if u.Service.ExecStart != "/usr/bin/foo --flag1 --flag2" {
		t.Fatalf("ExecStart = %q", u.Service.ExecStart)
	}
}

func TestContinuationEscapedBackslash(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.service")
	// Trailing double backslash is literal, not a continuation.
	content := "[Unit]\nDescription=x\n[Service]\nExecStart=/bin/echo hi\\\\\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	u, err := ParseUnit(p)
	if err != nil {
		t.Fatalf("ParseUnit: %v", err)
	}
	if u.Service.ExecStart != `/bin/echo hi\\` {
		t.Fatalf("ExecStart = %q", u.Service.ExecStart)
	}
}
