module initd

go 1.24.3

require (
	github.com/godbus/dbus/v5 v5.2.2
	github.com/google/shlex v0.0.0-20191202100458-e7afc7fbc510
	golang.org/x/sys v0.40.0
)

// Local fork of godbus v5.2.2 with one decoder fix: arrays of UNIX fds
// (signature "ah") are decoded as []UnixFD when the transport supplied the
// descriptors. Upstream appends UnixFD to a []UnixFDIndex, which panics with a
// plain string that Decode swallows, silently truncating the message. logind's
// StartTransientUnit carries session PIDs this way ("PIDFDs").
replace github.com/godbus/dbus/v5 => ./third_party/godbus
