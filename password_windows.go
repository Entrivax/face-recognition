//go:build windows

// Terminal handling for `recogn hash-password` on Windows: read the admin
// password from the console with echo disabled so it never appears on
// screen. The shared line reader lives in main.go.

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// stdinIsTerminal reports whether stdin is a console handle, so the
// hash-password command can prompt with hidden input.
func stdinIsTerminal() bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(os.Stdin.Fd()), &mode) == nil
}

// readPassword reads one line from the console with echo disabled. Console
// state is always restored; when the mode cannot be changed it falls back to
// a plain (echo-visible) line read rather than failing.
func readPassword() ([]byte, error) {
	h := windows.Handle(os.Stdin.Fd())
	var old uint32
	if err := windows.GetConsoleMode(h, &old); err != nil {
		return readPasswordLine()
	}
	if err := windows.SetConsoleMode(h, old&^windows.ENABLE_ECHO_INPUT); err != nil {
		return readPasswordLine()
	}
	defer windows.SetConsoleMode(h, old) //nolint:errcheck // restore best-effort
	b, err := readPasswordLine()
	if err == nil {
		fmt.Fprintln(os.Stderr) // newline after the hidden input
	}
	return b, err
}
