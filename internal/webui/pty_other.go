//go:build !linux

package webui

import "fmt"

func startPTY(_ string, _, _ int) (ptySession, error) {
	return nil, fmt.Errorf("native PTY terminal is supported only on Linux")
}
