//go:build linux

package cloud

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// readSecret disables the terminal's echo around one line read.
//
// Linux is the platform Otter runs on, and the stdlib syscall package exposes
// the termios ioctls directly, so no dependency is added for one prompt. When
// stdin is not a terminal the ioctl fails and the line is read with echo on,
// which is the right behaviour for a pipe.
func readSecret(in *os.File, out io.Writer) (string, error) {
	fd := uintptr(in.Fd())

	var old syscall.Termios
	hidden := false
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd,
		uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&old)), 0, 0, 0); errno == 0 {
		state := old
		state.Lflag &^= syscall.ECHO
		if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd,
			uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&state)), 0, 0, 0); errno == 0 {
			hidden = true
			defer func() {
				_, _, _ = syscall.Syscall6(syscall.SYS_IOCTL, fd,
					uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&old)), 0, 0, 0)
			}()
		}
	}

	line, err := bufio.NewReader(in).ReadString('\n')
	if hidden {
		// Echo was off, so the newline was not printed either. Put one back or
		// the next line of output starts on the prompt.
		fmt.Fprintln(out)
	}
	if err != nil && strings.TrimSpace(line) == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
