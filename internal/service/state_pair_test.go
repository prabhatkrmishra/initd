package service

import "testing"

// TestStatePair checks the two-axis mapping every reporter shares: getting
// SubState wrong makes tools that grep for "running"/"exited"/"dead" see
// nothing, because ActiveState alone is being echoed into both columns.
func TestStatePair(t *testing.T) {
	cases := []struct {
		state        State
		remainActive bool
		active, sub  string
	}{
		{StateActive, false, "active", "running"},
		{StateActive, true, "active", "exited"},
		{StateActivating, false, "activating", "start"},
		{StateStopping, false, "deactivating", "stop"},
		{StateFailed, false, "failed", "failed"},
		{StateInactive, false, "inactive", "dead"},
	}
	for _, c := range cases {
		active, sub := StatePair(c.state, c.remainActive)
		if active != c.active || sub != c.sub {
			t.Errorf("StatePair(%q, remainActive=%v) = %s/%s, want %s/%s",
				c.state, c.remainActive, active, sub, c.active, c.sub)
		}
	}
}
