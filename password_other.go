//go:build !linux && !windows

// Fallback for platforms without a dedicated terminal implementation: the
// hash-password command always reads the password from stdin (pipe or an
// echo-visible line) instead of prompting with hidden input.

package main

import "os"

// stdinIsTerminal reports that the interactive prompt is unavailable, so
// runHashPassword reads the password from stdin without terminal changes.
func stdinIsTerminal() bool { return false }

// readPassword falls back to a plain line read; it is only reached when
// stdinIsTerminal lied, so keep the echo-visible behaviour harmless.
func readPassword() ([]byte, error) { return readPasswordLine() }
