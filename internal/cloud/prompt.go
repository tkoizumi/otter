package cloud

import (
	"io"
	"os"
)

// ReadSecret reads one line from the terminal with echo disabled, so an Otter
// Cloud token never appears in a scrollback or a screen recording.
//
// The caller writes the prompt; this only owns the part that must not be
// echoed. It is a variable-free function rather than an inline read so the
// platform-specific termios handling lives in one file per platform and the
// rest of the package never sees it.
func ReadSecret(in *os.File, out io.Writer) (string, error) {
	return readSecret(in, out)
}
