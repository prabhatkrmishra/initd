package supervisor

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ErrNotScopeUnit marks names that are not .scope units. Callers map it to
// systemd's NotSupported.
var ErrNotScopeUnit = errors.New("only .scope transient units are supported")

// ErrBadScopeName marks names that are not usable as a unit file name or a
// cgroup leaf.
var ErrBadScopeName = errors.New("invalid scope unit name")

// ValidateTransientScopeName checks that name is safe to use as a unit file
// name and as a cgroup leaf: a plain ".scope" with no path components and a
// conservative character set, no leading dot, bounded length.
func ValidateTransientScopeName(name string) error {
	if !strings.HasSuffix(name, ".scope") {
		return fmt.Errorf("%w, got %q", ErrNotScopeUnit, name)
	}
	base := strings.TrimSuffix(name, ".scope")
	if base == "" || strings.Trim(base, ".") == "" {
		return fmt.Errorf("%w: empty base name", ErrBadScopeName)
	}
	if len(name) > 256 {
		return fmt.Errorf("%w: at most 256 characters", ErrBadScopeName)
	}
	for i, r := range name {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
		case strings.ContainsRune("-_.\\@:", r):
		default:
			return fmt.Errorf("%w: invalid character %q", ErrBadScopeName, r)
		}
		if i == 0 && r == '.' {
			return fmt.Errorf("%w: leading dot", ErrBadScopeName)
		}
	}
	return nil
}
