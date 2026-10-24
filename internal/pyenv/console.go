package pyenv

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// relocateConsoleScripts rewrites the shebang of console scripts in envDir/bin
// that point at a managed-Python interpreter, so a prepared environment keeps
// working after its data directory moves (a restore, or a placement change).
//
// What a console script is. When a package installs an entry point, pip/uv
// writes an executable whose first line is the absolute path of the
// environment's interpreter. The readiness marker can store its interpreter
// relatively, but a shebang cannot: it is an absolute path the kernel resolves
// before the script's own code runs, so after the data directory moves it names
// a file that no longer exists and executing the script fails with ENOENT.
//
// What is rewritten, and what is not. Only the first line, and only when the
// named interpreter is an absolute python* path inside an `environments`
// segment -- the managed layout this package owns. A script that deliberately
// names `/bin/sh` or the host's `/usr/bin/python3` is left byte-for-byte alone.
// Arguments after the interpreter (`#!/path/python -u`) are preserved.
//
// The rewrite is idempotent: a script whose shebang already names interpreter is
// not written at all, so a run does not touch the environment's mtimes. A write
// creates a temporary file beside the script, applies the original mode, and
// renames, so a crash cannot leave a half-written executable.
func relocateConsoleScripts(envDir, interpreter string) error {
	bin := filepath.Join(envDir, "bin")
	entries, err := os.ReadDir(bin)
	if err != nil {
		if os.IsNotExist(err) {
			// An environment without a bin directory has no console scripts.
			return nil
		}
		return fmt.Errorf("read console scripts in %s: %w", bin, err)
	}
	for _, entry := range entries {
		// A directory is not a script, and the environment's own `python`
		// symlink must never be replaced by a copy.
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		path := filepath.Join(bin, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat console script %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read console script %s: %w", path, err)
		}
		newline := strings.IndexByte(string(body), '\n')
		if newline < 0 || !strings.HasPrefix(string(body), "#!") {
			continue
		}
		line := string(body[2:newline])
		fields := strings.Fields(strings.TrimSuffix(line, "\r"))
		if len(fields) == 0 || !isManagedInterpreter(fields[0], interpreter) {
			continue
		}
		shebang := "#!" + interpreter
		if len(fields) > 1 {
			shebang += " " + strings.Join(fields[1:], " ")
		}
		rewritten := append([]byte(shebang+"\n"), body[newline+1:]...)
		if err := replaceFile(path, rewritten, info.Mode().Perm()); err != nil {
			return fmt.Errorf("relocate console script %s: %w", path, err)
		}
	}
	return nil
}

// isManagedInterpreter reports whether a shebang's interpreter is ours to
// rewrite: the environment's own interpreter already names it is not, anything
// that is not an absolute python* inside an `environments` segment is not.
func isManagedInterpreter(shebangPath, interpreter string) bool {
	if shebangPath == interpreter {
		return false
	}
	if !filepath.IsAbs(shebangPath) {
		return false
	}
	if !strings.HasPrefix(filepath.Base(shebangPath), "python") {
		return false
	}
	for _, part := range strings.Split(filepath.Clean(filepath.Dir(shebangPath)), string(filepath.Separator)) {
		if part == "environments" {
			return true
		}
	}
	return false
}

// replaceFile writes body to path through a temporary file in the same
// directory, so the replacement is atomic and keeps the original mode.
func replaceFile(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".relocate-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
