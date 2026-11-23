//go:build !unix

package mcp

import "os/exec"

func configureProcessGroup(_ *exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
