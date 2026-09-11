//go:build linux

// Terminal handling for `recogn hash-password`: read the admin password from
// an interactive terminal with echo disabled so it never appears on screen.
// Split out per platform like ort_embed_*.go; the shared line reader lives
// in main.go.

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// stdinIsTerminal reports whether stdin is an interactive terminal, so the
// hash-password command can prompt instead of reading a pipe.
func stdinIsTerminal() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TCGETS)
	return err == nil
}

// readPassword reads one line from the terminal with echo disabled. Terminal
// state is always restored; when the settings cannot be changed (redirected
// stdin, restricted environment) it falls back to a plain echo-visible read
// rather than failing.
func readPassword() ([]byte, error) {
	fd := int(os.Stdin.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return readPasswordLine()
	}
	noEcho := *old
	noEcho.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &noEcho); err != nil {
		return readPasswordLine()
	}
	defer unix.IoctlSetTermios(fd, unix.TCSETS, old) //nolint:errcheck // restore best-effort
	b, err := readPasswordLine()
	if err == nil {
		fmt.Fprintln(os.Stderr) // newline after the hidden input
	}
	return b, err
}
