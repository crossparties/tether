//go:build linux

package webui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type linuxPTY struct {
	master *os.File
	cmd    *exec.Cmd
	done   chan struct{}

	closeOnce sync.Once
}

func startPTY(workspace string, cols, rows int) (ptySession, error) {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	if strings.TrimSpace(workspace) == "" {
		workspace = "."
	}

	masterFD, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	master := os.NewFile(uintptr(masterFD), "/dev/ptmx")

	cleanupMaster := true
	defer func() {
		if cleanupMaster {
			_ = master.Close()
		}
	}()

	if err := unix.IoctlSetPointerInt(masterFD, unix.TIOCSPTLCK, 0); err != nil {
		return nil, fmt.Errorf("unlock pty slave (TIOCSPTLCK): %w", err)
	}
	ptyNum, err := unix.IoctlGetInt(masterFD, unix.TIOCGPTN)
	if err != nil {
		return nil, fmt.Errorf("query pty number (TIOCGPTN): %w", err)
	}
	ptsPath := fmt.Sprintf("/dev/pts/%d", ptyNum)

	slaveFD, err := unix.Open(ptsPath, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open pty slave %s: %w", ptsPath, err)
	}
	slave := os.NewFile(uintptr(slaveFD), ptsPath)
	defer func() {
		_ = slave.Close()
	}()

	_ = unix.IoctlSetWinsize(masterFD, unix.TIOCSWINSZ, &unix.Winsize{
		Row: uint16(clampDim(rows)),
		Col: uint16(clampDim(cols)),
	})

	shell := resolveShellBinary()
	cmd := exec.Command(shell)
	cmd.Dir = workspace
	cmd.Env = sanitizePTYEnv(workspace)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start shell %s: %w", shell, err)
	}

	cleanupMaster = false
	p := &linuxPTY{
		master: master,
		cmd:    cmd,
		done:   make(chan struct{}),
	}
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

func resolveShellBinary() string {
	if sh := strings.TrimSpace(os.Getenv("SHELL")); sh != "" {
		if info, err := os.Stat(sh); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			return sh
		}
	}
	for _, candidate := range []string{"/bin/bash", "/bin/sh"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return "/bin/sh"
}

func (p *linuxPTY) Read(b []byte) (int, error) {
	return p.master.Read(b)
}

func (p *linuxPTY) Write(b []byte) (int, error) {
	return p.master.Write(b)
}

func (p *linuxPTY) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("invalid terminal dimensions %dx%d", cols, rows)
	}
	rc, err := p.master.SyscallConn()
	if err != nil {
		return err
	}
	ws := &unix.Winsize{
		Row: uint16(clampDim(rows)),
		Col: uint16(clampDim(cols)),
	}
	var ioctlErr error
	if err := rc.Control(func(fd uintptr) {
		ioctlErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, ws)
	}); err != nil {
		return err
	}
	return ioctlErr
}

func (p *linuxPTY) Close() error {
	var closeErr error
	p.closeOnce.Do(func() {
		if p.cmd != nil && p.cmd.Process != nil && p.cmd.Process.Pid > 0 {
			pid := p.cmd.Process.Pid
			_ = syscall.Kill(-pid, syscall.SIGHUP)
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = p.cmd.Process.Kill()
		}
		if p.master != nil {
			closeErr = p.master.Close()
		}
		if p.done != nil {
			select {
			case <-p.done:
			case <-time.After(1 * time.Second):
			}
		}
	})
	return closeErr
}

func clampDim(v int) int {
	if v < 1 {
		return 1
	}
	if v > 500 {
		return 500
	}
	return v
}
