package main

import (
	"initd/internal/userpaths"
	"testing"
)

func TestPrimarySocketIsUserScoped(t *testing.T) {
	if !primaryIsUserSocket(userpaths.UserSocketPath()) {
		t.Fatal("the user socket must be served by the user manager")
	}
	if primaryIsUserSocket("/run/initd.sock") {
		t.Fatal("the system socket must be served by the system manager")
	}
}
