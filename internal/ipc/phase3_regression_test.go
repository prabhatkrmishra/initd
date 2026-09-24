package ipc

import (
	"testing"
)

func TestAbstractDialFallback(t *testing.T) {
	if _, ok := abstractDialFallback("@initd-user-1.sock"); ok {
		t.Fatalf("abstract input must not fallback")
	}
	if _, ok := abstractDialFallback("/run/initd.sock"); ok {
		t.Fatalf("short path must not fallback")
	}
	longUser := "/run/user/1000/very-long-runtime-dir-that-pushes-the-socket-path-well-past-ninety-chars-xyz/initd.sock"
	fb, ok := abstractDialFallback(longUser)
	if !ok || len(fb) == 0 || fb[0] != 0 {
		t.Fatalf("long user path should fallback to abstract, got %q %v", fb, ok)
	}
	longSys := "/run/user/1000/very-long-runtime-dir-that-pushes-the-socket-path-well-past-ninety-chars-xyz/initd-system.sock"
	fb2, ok := abstractDialFallback(longSys)
	if !ok {
		t.Fatalf("long system path should fallback")
	}
	if fb == fb2 {
		t.Fatalf("user vs system fallbacks must differ: %q", fb)
	}
}
