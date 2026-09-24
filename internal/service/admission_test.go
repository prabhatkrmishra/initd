package service

import (
	"testing"

	"initd/internal/parser"
)

func TestAdmitStartBurst(t *testing.T) {
	u := NewUnit(&parser.Unit{Name: "a.service"}, "")
	u.GetConfig().StartLimitIntervalSec = "60s"
	u.GetConfig().StartLimitBurst = "2"
	if err := u.AdmitStart(); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	if err := u.AdmitStart(); err != nil {
		t.Fatalf("second admit: %v", err)
	}
	if err := u.AdmitStart(); err == nil {
		t.Fatalf("third admit over burst=2 should fail")
	}
	if snap := u.Snapshot(); snap.State != StateFailed {
		t.Fatalf("over-budget admit should mark failed, got %v", snap.State)
	}
}

func TestDirectiveValidation(t *testing.T) {
	u := NewUnit(&parser.Unit{Name: "a.service"}, "")
	u.GetConfig().Service.UMask = "bogus"
	if _, err := u.buildExecCommand([]string{"/bin/true"}, commandOptions{}); err == nil {
		t.Fatalf("bad UMask should fail build")
	}
	u.GetConfig().Service.UMask = ""
	u.GetConfig().Service.LimitNOFILE = "bogus"
	if _, err := u.buildExecCommand([]string{"/bin/true"}, commandOptions{}); err == nil {
		t.Fatalf("bad LimitNOFILE should fail build")
	}
	u.GetConfig().Service.LimitNOFILE = "65536"
	u.GetConfig().Service.UMask = "022"
	if _, err := u.buildExecCommand([]string{"/bin/true"}, commandOptions{}); err != nil {
		t.Fatalf("valid directives should build: %v", err)
	}
}
