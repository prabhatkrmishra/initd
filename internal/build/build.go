// Package build carries version identity for every initd binary. It exists so
// a running daemon can be traced to a source commit: with the version repeated
// per binary, a rebuilt tree and a stale install were indistinguishable.
package build

// Version is the release line, overridden at build time with
// -X initd/internal/build.Version=$(VERSION).
var Version = "1.1.0"

// Meta records the exact source a binary came from, stamped by the Makefile
// (git commit, with -dirty for an uncommitted tree). Empty for hand-run
// `go build`, in which case String() reports the release line alone.
var Meta = ""

// BuildDate is the UTC timestamp of the build, stamped by the Makefile.
var BuildDate = ""

// String renders the version line shared by --version output and the D-Bus
// Version property, so a client can tell which commit is answering.
func String() string {
	if Meta == "" {
		return Version
	}
	if BuildDate == "" {
		return Version + "+" + Meta
	}
	return Version + "+" + Meta + ", built " + BuildDate
}
