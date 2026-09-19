package service

import "testing"

func TestIgnoredSecurityNotes(t *testing.T) {
	u := newTestUnit(t, "web.service", "[Service]\nExecStart=/bin/true\nPrivateTmp=yes\nProtectSystem=strict\nNoNewPrivileges=yes\nMemoryMax=100M\n")
	notes := u.IgnoredSecurityNotes()
	if len(notes) != 3 {
		t.Fatalf("notes = %v, want 3 sandbox warnings", notes)
	}
	for _, want := range []string{"PrivateTmp", "ProtectSystem", "NoNewPrivileges"} {
		found := false
		for _, n := range notes {
			if len(n) >= len(want) && n[:len(want)] == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing warning for %s in %v", want, notes)
		}
	}
	// Accounting knobs must stay silent.
	for _, n := range notes {
		if len(n) >= 9 && n[:9] == "MemoryMax" {
			t.Errorf("MemoryMax should not warn: %q", n)
		}
	}
}

func TestIgnoredSecurityNotesSortedAndPrefixed(t *testing.T) {
	got := IgnoredSecurityNotes(map[string]string{
		"Service.SystemCallFilter": "x",
		"Service.PrivateTmp":       "yes",
		"Unit.Description":         "y",
	})
	if len(got) != 2 {
		t.Fatalf("notes = %v, want 2", got)
	}
	if got[0] > got[1] {
		t.Fatalf("notes not sorted: %v", got)
	}
}

func TestStartWarnsIgnoredDirectives(t *testing.T) {
	u := newTestUnit(t, "web.service", "[Service]\nExecStart=/bin/sleep 30\nPrivateTmp=yes\n")
	if _, err := u.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	found := false
	for _, e := range u.Logs.Entries() {
		if len(e.Message) >= 8 && e.Message[:8] == "Warning:" {
			found = true
			break
		}
	}
	_ = u.Stop(u.StopTimeout())
	if !found {
		t.Fatal("start should log a warning for PrivateTmp")
	}
}
