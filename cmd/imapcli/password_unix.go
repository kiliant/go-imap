//go:build linux || darwin || freebsd || netbsd || openbsd

package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"unsafe"
)

// readPasswordPrompt reads one line from the terminal without echo.
func readPasswordPrompt(prompt string) (string, error) {
	f, err := passwordFile()
	if err != nil {
		return "", err
	}
	defer f.Close()

	fd := int(f.Fd())
	old, err := getTermios(fd)
	if err != nil {
		return "", fmt.Errorf("terminal: %w (set -pass or IMAP_PASS for non-interactive use)", err)
	}
	newState := old
	newState.Lflag &^= syscall.ECHO
	newState.Lflag |= syscall.ICANON | syscall.ISIG
	newState.Iflag |= syscall.ICRNL
	if err := setTermios(fd, &newState); err != nil {
		return "", err
	}
	defer setTermios(fd, &old)

	// ISIG stays on so Ctrl-C still interrupts, but the default action would
	// kill the process before the deferred restore runs and leave the shell
	// with echo off. Catch the signal, restore the terminal, then exit.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-signals:
			_ = setTermios(fd, &old)
			_, _ = fmt.Fprint(f, "\n")
			os.Exit(130)
		case <-done:
		}
	}()

	if _, err := fmt.Fprint(f, prompt); err != nil {
		return "", err
	}
	line, err := readPasswordLine(fd)
	_, _ = fmt.Fprint(f, "\n")
	if err != nil {
		return "", err
	}
	return trimPasswordLine(string(line)), nil
}

func passwordFile() (*os.File, error) {
	if isCharDevice(os.Stdin) {
		// Dup so Close in the caller does not close process stdin.
		fd, err := syscall.Dup(int(os.Stdin.Fd()))
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), "stdin"), nil
	}
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/tty: %w (set -pass or IMAP_PASS for non-interactive use)", err)
	}
	return f, nil
}

func isCharDevice(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// readPasswordLine reads until newline or NUL, matching golang.org/x/term.
func readPasswordLine(fd int) ([]byte, error) {
	var ret []byte
	var buf [1]byte
	for {
		n, err := syscall.Read(fd, buf[:])
		if n > 0 {
			switch buf[0] {
			case '\n', '\r':
				return ret, nil
			case 0:
				return ret, nil
			case 127, 8: // DEL / BS
				if len(ret) > 0 {
					ret = ret[:len(ret)-1]
				}
			default:
				ret = append(ret, buf[0])
			}
			continue
		}
		if err != nil {
			return ret, err
		}
		if n == 0 {
			if len(ret) == 0 {
				return nil, fmt.Errorf("EOF")
			}
			return ret, nil
		}
	}
}

func getTermios(fd int) (syscall.Termios, error) {
	var state syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), ioctlReadTermios, uintptr(unsafe.Pointer(&state)), 0, 0, 0)
	if errno != 0 {
		return state, errno
	}
	return state, nil
}

func setTermios(fd int, state *syscall.Termios) error {
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), ioctlWriteTermios, uintptr(unsafe.Pointer(state)), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
