package service

import "testing"

func TestExecStartBinaries(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/usr/sbin/sshd -D", "sshd"},
		{"/usr/sbin/mydaemon --state=/var/lib/mydaemon/state", "mydaemon"},
		{"@-/usr/sbin/sshd -D", "sshd"},
		{"-/usr/bin/foo bar", "foo"},
		{"FOO=1 /usr/bin/bar --x", "bar"},
		{"", ""},
	}
	for _, c := range cases {
		got := execStartBinaries(c.in)
		if c.want == "" {
			if len(got) != 0 {
				t.Fatalf("execStartBinaries(%q) = %v, want empty", c.in, got)
			}
			continue
		}
		if len(got) == 0 || got[0] != c.want {
			t.Fatalf("execStartBinaries(%q) = %v, want basename %q", c.in, got, c.want)
		}
	}
}
