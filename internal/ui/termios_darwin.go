//go:build darwin

package ui

import (
	"os"
	"syscall"
	"unsafe"
)

type winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

// MakeRaw puts f into raw terminal mode using macOS/BSD TIOCGETA/TIOCSETA ioctls
// and returns a restore function that resets the original termios state.
func MakeRaw(f *os.File) (func(), error) {
	if f == nil {
		return nil, syscall.EINVAL
	}
	fd := f.Fd()

	var old syscall.Termios
	if _, _, errno := syscall.Syscall6(
		syscall.SYS_IOCTL,
		fd,
		uintptr(syscall.TIOCGETA),
		uintptr(unsafe.Pointer(&old)),
		0, 0, 0,
	); errno != 0 {
		return nil, errno
	}

	raw := old
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0

	if _, _, errno := syscall.Syscall6(
		syscall.SYS_IOCTL,
		fd,
		uintptr(syscall.TIOCSETA),
		uintptr(unsafe.Pointer(&raw)),
		0, 0, 0,
	); errno != 0 {
		return nil, errno
	}

	restore := func() {
		_, _, _ = syscall.Syscall6(
			syscall.SYS_IOCTL,
			fd,
			uintptr(syscall.TIOCSETA),
			uintptr(unsafe.Pointer(&old)),
			0, 0, 0,
		)
	}
	return restore, nil
}

// TerminalSize queries the terminal dimensions via TIOCGWINSZ and falls back to (80, 24).
func TerminalSize(f *os.File) (width, height int) {
	if f == nil {
		return 80, 24
	}
	var ws winsize
	if _, _, errno := syscall.Syscall6(
		syscall.SYS_IOCTL,
		f.Fd(),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(&ws)),
		0, 0, 0,
	); errno != 0 || ws.Col == 0 || ws.Row == 0 {
		return 80, 24
	}
	return int(ws.Col), int(ws.Row)
}
