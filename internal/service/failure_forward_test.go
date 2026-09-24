package service

import (
	"testing"

	"initd/internal/parser"
)

func TestAllowFailureForwardBurst(t *testing.T) {
	u := NewUnit(&parser.Unit{Name: "f.service"}, "")
	u.GetConfig().StartLimitIntervalSec = "10s"
	u.GetConfig().StartLimitBurst = "2"
	if !u.AllowFailureForward() || !u.AllowFailureForward() {
		t.Fatalf("first two forwards should be allowed")
	}
	if u.AllowFailureForward() {
		t.Fatalf("third forward over burst=2 must be suppressed")
	}
}

func TestAllowFailureForwardUnlimited(t *testing.T) {
	u := NewUnit(&parser.Unit{Name: "f.service"}, "")
	u.GetConfig().StartLimitIntervalSec = "0"
	for i := 0; i < 10; i++ {
		if !u.AllowFailureForward() {
			t.Fatalf("unlimited budget must always allow (i=%d)", i)
		}
	}
}
