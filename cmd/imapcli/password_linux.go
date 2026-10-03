//go:build linux

package main

import "syscall"

// TCGETS and TCSETS differ between Linux architectures (ppc64 and mips do not
// use the x86 values), so take them from syscall rather than spelling them out.
const (
	ioctlReadTermios  = uintptr(syscall.TCGETS)
	ioctlWriteTermios = uintptr(syscall.TCSETS)
)
