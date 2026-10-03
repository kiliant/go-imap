//go:build darwin || freebsd || netbsd || openbsd

package main

import "syscall"

const (
	ioctlReadTermios  = uintptr(syscall.TIOCGETA)
	ioctlWriteTermios = uintptr(syscall.TIOCSETA)
)
