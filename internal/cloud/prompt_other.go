//go:build !linux

package cloud

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// readSecret is the fallback for platforms without the termios handling in
// prompt_linux.go. It reads the line plainly: the prompt is a convenience, and
// a developer on another platform is better served by a slightly visible token
// than by a package that does not build.
func readSecret(in *os.File, out io.Writer) (string, error) {
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
