package service

import (
	"os"
	"testing"

	"initd/internal/parser"
)

func TestUnsetEnvironment(t *testing.T) {
	t.Setenv("UNSET_ME_PHASE9", "leaked")
	t.Setenv("KEEP_ME_PHASE9", "kept")
	u := NewUnit(&parser.Unit{Name: "e.service"}, "")
	u.GetConfig().Service.UnsetEnvironment = []string{"UNSET_ME_PHASE9", "MISSING_NOOP"}
	u.GetConfig().Service.Environment = []string{"EXTRA_PHASE9=1"}
	envMap, _, err := u.buildEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := envMap["UNSET_ME_PHASE9"]; ok {
		t.Fatalf("UnsetEnvironment did not remove var")
	}
	if envMap["KEEP_ME_PHASE9"] != "kept" || envMap["EXTRA_PHASE9"] != "1" {
		t.Fatalf("other vars damaged: keep=%q extra=%q", envMap["KEEP_ME_PHASE9"], envMap["EXTRA_PHASE9"])
	}
	_ = os.Getpid()
}
