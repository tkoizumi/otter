package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// `otter init` scaffolds a workspace: the directory that holds integrations and
// the state a runtime keeps for them. Everything it writes is a plain file a
// developer is expected to edit, and nothing it writes is load-bearing for the
// runtime -- with one exception, the workspace marker, which is what
// `otter start` and the CLI's discovery walk look for.
//
// What it deliberately does not do: fetch anything, validate credentials,
// touch a daemon, or offer a gallery of vendor templates. An integration is a
// directory with a manifest and a Python file, so the scaffold is small on
// purpose.

// versionFileName records which runtime created the workspace, in the state
// directory next to the marker.
const versionFileName = "version"

// workspaceEnvFileName is the daemon-side environment file `otter start`
// loads. It is written as an example for the same reason the manifest does not
// carry secrets: real credentials must not land in a file the scaffold created
// and a developer forgot about.
//
// The template is a local convenience, not a committed one. The scaffolded
// .gitignore ignores env templates as well as real env files, because git
// cannot tell a filled-in example from a real file; the file exists to be
// copied to otter.env, and a fresh clone does not need it.
const workspaceEnvFileName = "otter.env.example"

// gitignoreFileName is written only when the workspace does not already have
// one, because appending to a project's ignore rules is not a scaffold's job.
const gitignoreFileName = ".gitignore"

// integrationNamePattern is the runtime's own manifest name rule, verbatim:
// lowercase letters, digits, dot, dash and underscore, starting and ending
// alphanumeric. It is duplicated here rather than shared because init has to
// reject a name before writing a manifest that validation would reject -- but
// it must not be *stricter* than validation, or init refuses names the runtime
// accepts.
var integrationNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)

// scaffoldFile is one file `init` may write.
type scaffoldFile struct {
	path    string
	content string
	mode    os.FileMode
	// skipIfPresent is for files that belong to the workspace rather than to
	// the integration: an existing .gitignore or env file is the developer's.
	skipIfPresent bool
}

// cmdInit implements `otter init`.
func (a *App) cmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	force := fs.Bool("force", false, "overwrite files that already exist")
	fs.Usage = func() {
		fmt.Fprintf(a.Stderr, "Usage: otter init [--force] [name]\n\n")
		fmt.Fprintf(a.Stderr, "Creates a workspace and one integration in it:\n")
		fmt.Fprintf(a.Stderr, "  .otter/           the marker, and the runtime version that made it\n")
		fmt.Fprintf(a.Stderr, "  <name>/otter.yaml the manifest\n")
		fmt.Fprintf(a.Stderr, "  <name>/main.py    the entrypoint\n")
		fmt.Fprintf(a.Stderr, "  <name>/logic.py   pure logic, testable without a runtime\n")
		fmt.Fprintf(a.Stderr, "  <name>/tests/     `python3 -m unittest discover -s tests`\n\n")
		fmt.Fprintf(a.Stderr, "The name defaults to the current directory's name. Run it inside an existing\n")
		fmt.Fprintf(a.Stderr, "workspace to add another integration to it.\n\n")
		fmt.Fprintf(a.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(a.Stderr, "otter: init takes at most one name, got %v\n", fs.Args())
		fs.Usage()
		return 2
	}

	wd, err := workingDirForTest()
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: cannot determine the working directory: %v\n", err)
		return 1
	}

	name := ""
	if fs.NArg() == 1 {
		name = strings.TrimSpace(fs.Arg(0))
	}
	if name == "" {
		name = filepath.Base(wd)
	}
	if !integrationNamePattern.MatchString(name) {
		fmt.Fprintf(a.Stderr, "otter: %q is not a usable integration name\n", name)
		fmt.Fprintf(a.Stderr, "otter: use lowercase letters, digits, dot, dash or underscore,\n")
		fmt.Fprintf(a.Stderr, "otter: starting and ending with a letter or digit (for example tshirt-company)\n")
		return 2
	}

	root, created, code := initTarget(a.Stderr, wd)
	if code != 0 {
		return code
	}

	integrationDir := filepath.Join(root, name)
	if entries, err := os.ReadDir(integrationDir); err == nil && !*force {
		fmt.Fprintf(a.Stderr, "otter: %s already exists (%d entr%s)\n",
			relTo(root, integrationDir), len(entries), plural(len(entries), "y", "ies"))
		fmt.Fprintf(a.Stderr, "otter: pick another name, or pass --force to write over the scaffolded files\n")
		return 1
	}

	written, skipped, err := writeScaffold(root, name, a.Version, *force)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}

	if created {
		fmt.Fprintf(a.Stdout, "created workspace  %s\n", root)
	} else {
		fmt.Fprintf(a.Stdout, "workspace          %s (existing)\n", root)
	}
	fmt.Fprintf(a.Stdout, "integration        %s\n", name)
	fmt.Fprintf(a.Stdout, "runtime version    %s\n", a.Version)
	for _, path := range written {
		fmt.Fprintf(a.Stdout, "wrote              %s\n", relTo(root, path))
	}
	for _, path := range skipped {
		fmt.Fprintf(a.Stdout, "kept               %s (already existed)\n", relTo(root, path))
	}
	fmt.Fprintf(a.Stdout, "\nnext:\n")
	fmt.Fprintf(a.Stdout, "  otter validate %s\n", name)
	fmt.Fprintf(a.Stdout, "  otter release %s\n", name)
	fmt.Fprintf(a.Stdout, "  otter start --detach\n")
	fmt.Fprintf(a.Stdout, "  otter run %s\n", name)
	fmt.Fprintf(a.Stdout, "  otter stop\n")
	if _, running := runningURL(serveDir(root, "")); running {
		fmt.Fprintf(a.Stdout, "\nnote: a runtime is already serving this workspace; it discovered integrations\n")
		fmt.Fprintf(a.Stdout, "note: when it started, so run `otter reload` to pick this one up without\n")
		fmt.Fprintf(a.Stdout, "note: interrupting the integrations already running.\n")
	}
	return 0
}

// initTarget decides where a scaffold goes.
//
// Inside an existing workspace it adds an integration to that workspace without
// touching the marker. Outside one, it takes over the current directory -- but
// only if that directory is empty apart from dotfiles. Scaffolding into the
// middle of someone's repository would bury four files among their own, and
// `cd client && otter init client` is the shape that keeps this predictable.
func initTarget(stderr io.Writer, wd string) (root string, created bool, code int) {
	if found, ok := detectProjectRoot(wd); ok {
		return found, false, 0
	}
	entries, err := os.ReadDir(wd)
	if err != nil {
		fmt.Fprintf(stderr, "otter: cannot read %s: %v\n", wd, err)
		return "", false, 1
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".") {
			fmt.Fprintf(stderr, "otter: %s is not empty and is not a workspace\n", wd)
			fmt.Fprintf(stderr, "otter: run init in an empty directory, or in one that already has .otter\n")
			return "", false, 1
		}
	}
	return wd, true, 0
}

// writeScaffold writes the workspace marker and one integration. It returns the
// paths written and the paths it left alone because they already existed.
func writeScaffold(root, name, version string, force bool) (written, skipped []string, err error) {
	files := []scaffoldFile{
		{
			path:    filepath.Join(root, stateDirName, versionFileName),
			content: version + "\n",
			mode:    0o644,
			// The runtime that created the workspace is a fact about the first
			// init and does not change when an integration is added.
			skipIfPresent: true,
		},
		{
			path:    filepath.Join(root, workspaceEnvFileName),
			content: workspaceEnvExample(),
			mode:    0o644,
			// A real otter.env is the operator's file; never overwrite it.
			skipIfPresent: true,
		},
		{
			path:          filepath.Join(root, gitignoreFileName),
			content:       gitignoreTemplate(),
			mode:          0o644,
			skipIfPresent: true,
		},
		{
			path:    filepath.Join(root, name, "otter.yaml"),
			content: manifestTemplate(name),
			mode:    0o644,
		},
		{
			path:    filepath.Join(root, name, "main.py"),
			content: mainTemplate,
			mode:    0o644,
		},
		{
			path:    filepath.Join(root, name, "logic.py"),
			content: logicTemplate,
			mode:    0o644,
		},
		{
			path:    filepath.Join(root, name, "tests", "test_logic.py"),
			content: testTemplate,
			mode:    0o644,
		},
	}

	for _, file := range files {
		if file.skipIfPresent {
			if _, statErr := os.Stat(file.path); statErr == nil {
				skipped = append(skipped, file.path)
				continue
			}
		}
		if !force {
			if _, statErr := os.Stat(file.path); statErr == nil {
				return written, skipped, fmt.Errorf("%s already exists", relTo(root, file.path))
			}
		}
		if err := os.MkdirAll(filepath.Dir(file.path), 0o755); err != nil {
			return written, skipped, err
		}
		if err := os.WriteFile(file.path, []byte(file.content), file.mode); err != nil {
			return written, skipped, err
		}
		written = append(written, file.path)
	}
	return written, skipped, nil
}

// relTo keeps the output readable: paths relative to the workspace, or the
// absolute path when it is not below it.
func relTo(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}

// plural picks the suffix for a count, for messages like "3 entries".
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// projectVersion reads the runtime version a workspace was created with.
func projectVersion(root string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(root, stateDirName, versionFileName))
	if err != nil {
		return "", false
	}
	version := strings.TrimSpace(string(data))
	if version == "" {
		return "", false
	}
	return version, true
}

// warnVersionDrift reports a workspace created by a different runtime than the
// one about to serve it. It is a warning, not a refusal: pinning is the
// operator's decision, and the recorded version is a note the scaffold left,
// not a constraint the runtime enforces.
func warnVersionDrift(w io.Writer, root, current string) {
	if root == "" || current == "" || current == "dev" {
		return
	}
	recorded, ok := projectVersion(root)
	if !ok || recorded == current {
		return
	}
	fmt.Fprintf(w, "note: this workspace was created with otter %s; this is otter %s\n", recorded, current)
	fmt.Fprintf(w, "note: state migrates forward only, so do not point an older runtime at a newer workspace\n")
}

func manifestTemplate(name string) string {
	return "# Otter manifest. Every field is documented in docs/manifest-reference.md.\n" +
		"version: 1\n" +
		"\n" +
		"name: " + name + "\n" +
		"description: What this integration does.\n" +
		"\n" +
		"entrypoint: main.py\n" +
		"\n" +
		"# No trigger: manual runs only, so nothing fires while you work. Add one when\n" +
		"# the integration is ready, then restart the runtime to register it:\n" +
		"#\n" +
		"# trigger:\n" +
		"#   cron: \"*/5 * * * *\"\n" +
		"\n" +
		"# Secrets are read from the DAEMON's environment, never from this file:\n" +
		"# `otter inspect` prints everything here, so a secret belongs in the\n" +
		"# workspace's otter.env (which `otter start` loads) and is named here:\n" +
		"#\n" +
		"# secrets:\n" +
		"#   - " + secretName(name) + "\n" +
		"\n" +
		"# Non-secret, deployment-specific settings. Values may reference the daemon\n" +
		"# environment with ${VAR}.\n" +
		"env:\n" +
		"  LOG_LEVEL: info\n" +
		"\n" +
		"timeout: 30\n" +
		"\n" +
		"retry:\n" +
		"  attempts: 3\n" +
		"  backoff: exponential\n" +
		"  initial_delay: 5s\n"
}

// secretName renders the name of a plausible secret for the scaffolded
// integration, so the commented example reads as a real one.
func secretName(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_TOKEN"
}

const mainTemplate = `"""The entrypoint.

The runtime runs python3 main.py with this directory as the working directory
and the SDK's otter package already on PYTHONPATH. State, logs and the trigger
arrive on the context.
"""

from otter import Context

from logic import next_count

ctx = Context.from_environment()

count = next_count(ctx.state.get("count"))

ctx.log.info("ran", count=count, trigger=ctx.trigger.type)

ctx.state.set("count", count)
`

const logicTemplate = `"""Pure logic: no I/O, no environment reads, no SDK imports.

Keeping decisions here is what lets tests/test_logic.py run under plain
python3, with no runtime and no daemon. Add vendor clients and mapping code in
this directory as the integration grows; main.py stays the wiring.
"""


def next_count(current):
    """Return the value after current, treating a missing value as 0."""
    return (current or 0) + 1
`

const testTemplate = `"""Runtime-free tests.

No daemon, no SDK, no network: python3 -m unittest discover -s tests
"""

import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from logic import next_count  # noqa: E402


class NextCount(unittest.TestCase):
    def test_starts_at_one_when_nothing_is_stored(self):
        self.assertEqual(next_count(None), 1)

    def test_increments_what_is_stored(self):
        self.assertEqual(next_count(41), 42)


if __name__ == "__main__":
    unittest.main()
`

func workspaceEnvExample() string {
	return `# Credentials for the runtime daemon, loaded by otter start.
#
# One file for the whole workspace, because the daemon's environment is one
# process environment. An integration receives only the names its own manifest
# lists under secrets:.
#
# Copy to otter.env (gitignored) and fill in. Never put a value here that you
# would paste into a chat log, and never in an integration's otter.yaml --
# otter inspect prints that in full.
#
# ACME_TOKEN=
`
}

func gitignoreTemplate() string {
	return `# Runtime state: SQLite, extracted SDK, the serve record and its log
.otter/

# Secrets: never commit an env file, templates included. A .env.example that
# someone pasted real values into is still a secret, and git cannot tell the
# two apart. Keep placeholders out of any file you commit.
.env
.env.*
*.env
*.env.*

# Python
__pycache__/
*.py[cod]
.venv/
venv/
`
}
