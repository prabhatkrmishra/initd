package main

import (
	"reflect"
	"testing"
)

func TestSplitQuietFlag(t *testing.T) {
	rest, q := splitQuietFlag([]string{"-q", "foo"})
	if !q || !reflect.DeepEqual(rest, []string{"foo"}) {
		t.Fatalf("got %v %v", rest, q)
	}
	rest, q = splitQuietFlag([]string{"foo"})
	if q || !reflect.DeepEqual(rest, []string{"foo"}) {
		t.Fatalf("got %v %v", rest, q)
	}
}
