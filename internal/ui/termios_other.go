//go:build !linux && !darwin

package ui

import (
	"errors"
	"os"
)

// MakeRaw is a no-op fallback on unsupported platforms.
func MakeRaw(f *os.File) (func(), error) {
	return nil, errors.New("raw mode is not supported on this platform")
}

// TerminalSize returns standard 80x24 fallback dimensions on unsupported platforms.
func TerminalSize(f *os.File) (width, height int) {
	return 80, 24
}
