package main

import "os"

// isTerminal reports whether stdout is a character device. Paging only ever
// happens on a tty; pipes and redirects always get plain output.
func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
