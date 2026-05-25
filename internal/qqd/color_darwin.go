//go:build darwin

package qqd

import (
	"syscall"
	"unsafe"
)

func isTerminal(fd uintptr) bool {
	var termios [256]byte
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGETA), uintptr(unsafe.Pointer(&termios[0])))
	return err == 0
}
