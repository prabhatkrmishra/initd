package main

import "testing"

func TestParseShowArgs(t *testing.T) {
	cases := []struct {
		in            string
		args          []string
		wantProps     []string
		wantValue     bool
		wantUnits     []string
	}{
		{
			in:  "openclaw form: show --property=LoadState --value UNIT",
			args: []string{"--property=LoadState", "--value", "openclaw-gateway.service"},
			wantProps:    []string{"LoadState"},
			wantValue:    true,
			wantUnits:    []string{"openclaw-gateway.service"},
		},
		{
			in:  "no flags",
			args: []string{"foo.service"},
			wantProps:    nil,
			wantValue:    false,
			wantUnits:    []string{"foo.service"},
		},
		{
			in:  "comma-separated properties",
			args: []string{"--property=Id,ActiveState,SubState", "--value", "foo.service"},
			wantProps:    []string{"Id", "ActiveState", "SubState"},
			wantValue:    true,
			wantUnits:    []string{"foo.service"},
		},
		{
			in:  "unknown flag ignored",
			args: []string{"--no-pager", "--property=LoadState", "foo.service"},
			wantProps:    []string{"LoadState"},
			wantValue:    false,
			wantUnits:    []string{"foo.service"},
		},
		{
			in:  "empty",
			args: []string{},
			wantProps:    nil,
			wantValue:    false,
			wantUnits:    nil,
		},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			props, val, units := parseShowArgs(c.args)
			if len(props) != len(c.wantProps) {
				t.Fatalf("props=%v want %v", props, c.wantProps)
			}
			for i, p := range props {
				if p != c.wantProps[i] {
					t.Fatalf("props[%d]=%q want %q", i, p, c.wantProps[i])
				}
			}
			if val != c.wantValue {
				t.Fatalf("valueOnly=%v want %v", val, c.wantValue)
			}
			if len(units) != len(c.wantUnits) {
				t.Fatalf("units=%v want %v", units, c.wantUnits)
			}
			for i, u := range units {
				if u != c.wantUnits[i] {
					t.Fatalf("units[%d]=%q want %q", i, u, c.wantUnits[i])
				}
			}
		})
	}
}
