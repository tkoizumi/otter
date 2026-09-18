package cli

import (
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/tkoizumi/otter/internal/api"
)

// This file is how the CLI finds a project's daemon.
//
// The CLI is a client: without --api or OTTER_API_URL it assumes the default
// loopback address, which is right for a host running the production service
// and wrong for anyone developing locally on another port. A developer who
// started a daemon in one terminal should be able to type `otter run hello` in
// another, and they cannot be expected to export an environment variable first.
//
// So the daemon records the address it actually bound in its data directory,
// and the CLI looks for that file walking up from the working directory -- the
// way git finds a repository. The project directory becomes the unit of
// context, and the address of the daemon serving it stops being something a
// human has to carry around.
//
// The same walk identifies the project itself, which is what lets `otter
// start` default its integrations directory and its data directory without
// being told either one.

// ListenURLFileName is written by a running daemon into its data directory.
//
// It is deliberately not called "api.url" or "otter.url": the data directory
// is *inside* the project, and the state directory that discovery walks to is
// `<project>/.otter`. A name like `.otter/api.url` would collide with a data
// directory that is itself `.<something>/api.url`, so discovery would need to
// know each project's data layout. A distinct name cannot collide, and it
// remains findable from any directory in the project.
const ListenURLFileName = "listen.url"

// ServePIDFileName is written beside the listen file by the daemon it names,
// so `otter stop` can stop the runtime serving this project without knowing
// its port or its process name.
const ServePIDFileName = "serve.pid"

// ServeDataFileName records which data directory the named daemon owns. Without
// it a workspace cannot tell a runtime that is merely already running from a
// runtime serving a different state directory, and those need opposite
// answers: the first is success, the second is a refusal.
const ServeDataFileName = "serve.data"

// stateDirName is the directory that marks a project. Discovery looks for the
// listen file somewhere below it, so a project is identified the way git
// identifies a repository: by a dotted directory at its root.
const stateDirName = ".otter"

// maxDiscoveryDepth bounds the walk to the filesystem root. Deep enough for any
// real project nesting, shallow enough that a stray file in / cannot be found
// from an unrelated directory.
const maxDiscoveryDepth = 32

// workingDirForTest lets a test run discovery from a temporary directory.
var workingDirForTest = os.Getwd

// resolveAPI fills in g.api from the environment, then from the project, when
// the operator did not name a daemon. Precedence, highest first:
//
//  1. --api on the command line
//  2. OTTER_API_URL
//  3. the nearest <dir>/.otter/api.url, walking up from the working directory
//  4. the compiled-in default, left as the empty string
//
// The environment stays above discovery because an operator pointing every
// command at one daemon with an exported variable must not be silently
// redirected by whatever directory they happen to be in.
func resolveAPI(g *globals) bool {
	if g.api != "" {
		return true
	}
	if fromEnv := strings.TrimSpace(os.Getenv("OTTER_API_URL")); fromEnv != "" {
		g.api = fromEnv
		return true
	}
	dir, err := workingDirForTest()
	if err != nil {
		return false
	}
	// Every root from here upward, nearest first: a record left behind by a
	// stopped daemon must not hide a live one further up, and a dead record
	// must never become the answer.
	inProject := false
	for _, root := range projectRoots(dir) {
		inProject = true
		if base, _, ok := discoverAPIURL(root); ok && apiAnswers(base) {
			g.api = base
			return true
		}
	}
	return inProject
}

// projectRoots lists the directories from dir upward that carry a workspace
// marker, nearest first.
func projectRoots(dir string) []string {
	var roots []string
	dir = filepath.Clean(dir)
	for depth := 0; depth < maxDiscoveryDepth; depth++ {
		for _, marker := range projectMarkers {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				roots = append(roots, dir)
				break
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return roots
}

// discoverAPIURL walks up from dir looking for a project whose daemon recorded
// itself. It returns the URL and the project root that answered.
//
// The project's data directory is not at a fixed depth -- `--data` can point
// anywhere, and the conventional location is inside the project as well -- so
// the listen file is searched for below `.otter` rather than assumed to be
// directly inside it. That keeps the layout the developer's choice while
// keeping the lookup deterministic.
func discoverAPIURL(dir string) (string, string, bool) {
	dir = filepath.Clean(dir)
	for depth := 0; depth < maxDiscoveryDepth; depth++ {
		root := filepath.Join(dir, stateDirName)
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			if base, ok := findListenFile(root); ok {
				return base, dir, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // filesystem root
		}
		dir = parent
	}
	return "", "", false
}

// heavyStateDirs are pruned while looking for the listen file. They are the
// parts of a data directory that can hold thousands of files (an extracted
// SDK, prepared interpreters, staged releases), and none of them is where a
// daemon writes its address. Without this, discovery in a project with a
// prepared environment stats a few hundred thousand files.
var heavyStateDirs = map[string]bool{
	"sdk":          true,
	"python":       true,
	"releases":     true,
	"environments": true,
}

// findListenFile looks for listen.url below root, at any depth. It returns the
// first usable address it finds.
func findListenFile(root string) (string, bool) {
	var base string
	var found bool
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subdirectory is not a reason to give up: the
			// answer may be in one of its siblings.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if found {
			return fs.SkipDir
		}
		if d.IsDir() {
			if path != root && heavyStateDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() != ListenURLFileName {
			return nil
		}
		if addr, ok := readListenURLFile(path); ok {
			base, found = addr, true
		}
		return nil
	})
	return base, found
}

// readListenURLFile reads one recorded address. A missing or empty file is not an
// error: neither means anything a developer can act on, and the CLI has a
// usable default.
func readListenURLFile(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	base := canonicalAPIURL(strings.TrimSpace(string(data)))
	if base == "" {
		return "", false
	}
	return base, true
}

// canonicalAPIURL normalizes what a daemon wrote, and rejects anything that is
// not a bare http(s) address. A file that is not ours must not turn into a
// request to somewhere unexpected.
func canonicalAPIURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return ""
	}
	if u.Host == "" {
		return ""
	}
	// A trailing slash makes every derived path a double slash, which is
	// harmless but ugly in errors. The client joins paths itself.
	return strings.TrimSuffix(raw, "/")
}

// listenAPIURL turns a listen address into the URL clients should use. The
// daemon already binds loopback by default and refuses to bind elsewhere
// without a token, so an unspecified host means loopback here too.
func listenAPIURL(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return api.DefaultBaseURL
	}
	if strings.Contains(listen, "://") {
		return canonicalAPIURL(listen)
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		// Not host:port. Only something like ":7400" is still meaningful, and
		// anything else is a configuration the daemon's own validation has
		// already rejected.
		if strings.HasPrefix(listen, ":") {
			return canonicalAPIURL("127.0.0.1" + listen)
		}
		return ""
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return canonicalAPIURL(net.JoinHostPort(host, port))
}

// writeListenURLFile records the address a daemon bound, beside its data. It is
// best effort: a daemon that cannot write it still serves, and the developer
// falls back to --api.
func writeListenURLFile(dataDir, base string) error {
	if dataDir == "" || base == "" {
		return nil
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, ListenURLFileName), []byte(base+"\n"), 0o644)
}

// writeServeData records the data directory a daemon serves.
func writeServeData(recordDir, dataDir string) error {
	if recordDir == "" || dataDir == "" {
		return nil
	}
	if err := os.MkdirAll(recordDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(recordDir, ServeDataFileName), []byte(dataDir+"\n"), 0o644)
}

// readServeData reports the data directory the recorded daemon uses.
func readServeData(recordDir string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(recordDir, ServeDataFileName))
	if err != nil {
		return "", false
	}
	dir := strings.TrimSpace(string(data))
	if dir == "" {
		return "", false
	}
	return dir, true
}

// removeListenURLFile clears the record on a clean shutdown, so a stale address
// is never handed to the next command.
func removeListenURLFile(dataDir string) {
	if dataDir != "" {
		_ = os.Remove(filepath.Join(dataDir, ListenURLFileName))
	}
}
