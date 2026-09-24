package deploy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/tkoizumi/otter/internal/identity"
)

// Runner executes work against the remote host.
//
// Everything the deployment does remotely goes through this interface, which
// is what makes the converge testable without a server: the tests substitute a
// fake that records the scripts instead of running them.
type Runner interface {
	// RunStream runs a remote command, wiring its output to the deployment's
	// streams.
	RunStream(ctx context.Context, command string, stdout, stderr io.Writer) error

	// RunScript runs a script on the remote host, with the script supplied on
	// stdin. Secrets are only ever written to a host this way: a token in argv
	// is a token in that host's process table.
	RunScript(ctx context.Context, script string) error

	// Output runs a remote command and returns its stdout.
	Output(ctx context.Context, command string) (string, error)

	// Push copies a local tree into a directory on the remote host.
	Push(ctx context.Context, localDir, remoteDir string, extraArgs ...string) error

	// Close releases any pooled connection and temporary files.
	Close() error
}

// SSH is the real Runner: an OpenSSH ControlMaster plus rsync.
//
// The control master matters more than it looks. A deploy is a dozen or so
// round trips, and without connection sharing each one pays a fresh TCP
// handshake and key exchange. With it, the first connection is slow and the
// rest are instant.
type SSH struct {
	Target  Target
	Stdout  io.Writer
	Stderr  io.Writer
	Verbose bool

	dir        string
	masterPath string
	keyPath    string
	opened     bool
	useSudo    bool
	keyOnce    sync.Once
	keyErr     error
}

// NewSSH returns a Runner for a target.
func NewSSH(target Target, stdout, stderr io.Writer) *SSH {
	return &SSH{Target: target, Stdout: stdout, Stderr: stderr}
}

// sshBaseArgs builds the options shared by every ssh and rsync invocation.
func (s *SSH) sshBaseArgs() []string {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=15",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"-p", strconv.Itoa(s.Target.Port),
	}
	if s.keyPath != "" {
		args = append(args, "-i", s.keyPath)
	}
	if s.masterPath != "" {
		args = append(args,
			"-o", "ControlMaster=auto",
			"-o", "ControlPath="+s.masterPath,
			"-o", "ControlPersist=60s",
		)
	}
	return args
}

func (s *SSH) sshArgs() []string {
	return append(s.sshBaseArgs(), s.destination())
}

func (s *SSH) destination() string {
	if s.Target.User == "" {
		return s.Target.Host
	}
	return s.Target.User + "@" + s.Target.Host
}

// ShellCommand builds the remote shell invocation for a command passed as an
// argument.
//
// Logins that are not already root go through `sudo -n`, which fails
// immediately with a clear message rather than hanging on a password prompt
// that a non-interactive session can never satisfy. The command travels as an
// argument rather than on stdin because these invocations never write stdin,
// and a shell reading from an empty stdin would silently execute nothing.
func (s *SSH) ShellCommand(script string) string {
	if s.Target.LoginIsRoot() {
		return "sh -c " + ShellQuote(script)
	}
	return "sudo -n sh -c " + ShellQuote(script)
}

// ScriptCommand builds the remote shell invocation for a script supplied on
// stdin. This is the only form used for anything carrying a secret, because a
// secret in argv is a secret in the remote process table.
func (s *SSH) ScriptCommand() string {
	if s.Target.LoginIsRoot() {
		return "sh -s"
	}
	return "sudo -n sh -s"
}

// Open establishes the shared connection and confirms the target is reachable
// and that the login can escalate to root.
func (s *SSH) Open(ctx context.Context) error {
	if s.opened {
		return nil
	}

	dir, err := os.MkdirTemp("", "otter-deploy-")
	if err != nil {
		return fmt.Errorf("create temporary directory: %w", err)
	}
	s.dir = dir
	// ControlPath values are length limited on some systems, so keep it short.
	s.masterPath = dir + "/cm"

	if err := s.prepareKey(); err != nil {
		return err
	}

	script := "set -e\n" +
		"if [ \"$(id -u)\" -ne 0 ] && ! sudo -n true 2>/dev/null; then\n" +
		"  echo \"otter: this login is not root and has no passwordless sudo.\" >&2\n" +
		"  echo \"otter: systemd needs root; deploy as root or add a NOPASSWD sudo rule for $(whoami).\" >&2\n" +
		"  exit 1\n" +
		"fi\n"

	s.useSudo = !s.Target.LoginIsRoot()

	if err := s.RunScript(ctx, script); err != nil {
		return err
	}
	if err := s.checkTools(ctx); err != nil {
		return err
	}
	s.opened = true
	return nil
}

// requiredTools are the commands a deploy runs on the host. They are not
// universally present: a minimal Amazon Linux 2023 image has neither rsync nor
// find, and discovering that as "bash: rsync: command not found" in the middle
// of a sync is a poor way to learn it.
func (s *SSH) requiredTools() []string {
	// ss (iproute2) is how a new workspace finds a loopback port nothing is
	// listening on. Without it a deploy could hand a workspace a port already
	// in use, which surfaces later as an unexplained unhealthy daemon.
	tools := []string{"rsync", "systemctl", "install", "find", "chown", "ln", "getent", "groupadd", "useradd", "ss"}
	if !s.Target.LoginIsRoot() {
		// Preparation runs as the service account, so a non-root login needs a
		// way to drop privileges as well as to escalate.
		tools = append(tools, "runuser", "sudo")
	}
	return tools
}

// needsRemoteSudo reports whether remote commands must escalate. It mirrors
// ScriptCommand so a push and a script agree about privileges.
func (s *SSH) needsRemoteSudo() bool { return !s.Target.LoginIsRoot() }

// checkTools fails before anything is built or pushed when the host cannot run
// a deploy, and names the packages to install.
func (s *SSH) checkTools(ctx context.Context) error {
	script := `missing=""
for tool in ` + strings.Join(s.requiredTools(), " ") + `; do
  command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
if [ -n "$missing" ]; then
  echo "otter: the host is missing tools this deploy needs:$missing" >&2
  echo "otter: install them and retry, for example:" >&2
  echo "  Debian/Ubuntu:  apt-get install -y rsync findutils sudo" >&2
  echo "  Amazon Linux:   dnf install -y rsync findutils sudo" >&2
  echo "  RHEL/Rocky:     dnf install -y rsync findutils sudo" >&2
  exit 1
fi
`

	if err := s.RunScript(ctx, script); err != nil {
		return fmt.Errorf("check tools on %s: %w", s.destination(), err)
	}
	return nil
}

// prepareKey materializes the operator's key as a 0600 temporary file.
//
// rsync refuses a key file that is group or world readable, and some ssh
// clients insist on a path they can chmod themselves, so copying to a fresh
// private file avoids depending on how the operator stores theirs. It is done
// once and remembered, and it is not tied to Open: a deploy with a pinned
// platform never calls Open, but it still has to authenticate rsync.
func (s *SSH) prepareKey() error {
	if s.Target.IdentityFile == "" {
		return nil
	}
	s.keyOnce.Do(func() {
		if s.dir == "" {
			dir, err := os.MkdirTemp("", "otter-deploy-key-")
			if err != nil {
				s.keyErr = fmt.Errorf("create temporary directory: %w", err)
				return
			}
			s.dir = dir
		}
		data, err := os.ReadFile(s.Target.IdentityFile)
		if err != nil {
			s.keyErr = fmt.Errorf("read identity file: %w", err)
			return
		}
		path := filepath.Join(s.dir, "key")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			s.keyErr = fmt.Errorf("write temporary identity file: %w", err)
			return
		}
		s.keyPath = path
	})
	return s.keyErr
}

// Platform asks the remote host what it is, so a local guess is never trusted.
func (s *SSH) Platform(ctx context.Context) (string, error) {
	out, err := s.Output(ctx, "uname -s && uname -m")
	if err != nil {
		return "", err
	}
	lines := strings.Fields(out)
	if len(lines) < 2 {
		return "", fmt.Errorf("unexpected uname output %q", strings.TrimSpace(out))
	}
	return normalizePlatform(lines[0], lines[1])
}

func normalizePlatform(osName, machine string) (string, error) {
	var goos string
	switch strings.ToLower(osName) {
	case "linux":
		goos = "linux"
	case "darwin":
		goos = "darwin"
	default:
		return "", fmt.Errorf("unsupported remote operating system %q: otter deploy targets linux", osName)
	}

	var goarch string
	switch strings.ToLower(machine) {
	case "x86_64", "amd64":
		goarch = "amd64"
	case "aarch64", "arm64":
		goarch = "arm64"
	default:
		return "", fmt.Errorf("unsupported remote architecture %q: otter deploy builds amd64 or arm64", machine)
	}
	return goos + "/" + goarch, nil
}

// RunStream implements Runner.
func (s *SSH) RunStream(ctx context.Context, command string, stdout, stderr io.Writer) error {
	return s.exec(ctx, command, stdout, stderr)
}

// RunScript implements Runner.
func (s *SSH) RunScript(ctx context.Context, script string) error {
	if err := s.prepareKey(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "ssh", append(s.sshArgs(), s.ScriptCommand())...)
	cmd.Stdin = strings.NewReader(script)
	cmd.Stdout = s.Stdout
	cmd.Stderr = s.Stderr
	return s.wrap("ssh "+s.destination(), cmd.Run())
}

// Output implements Runner.
func (s *SSH) Output(ctx context.Context, command string) (string, error) {
	var stdout, stderr bytes.Buffer
	if err := s.exec(ctx, command, &stdout, &stderr); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return "", fmt.Errorf("remote command %q: %w", command, err)
		}
		return "", fmt.Errorf("remote command %q: %w\n%s", command, err, indent(msg))
	}
	return stdout.String(), nil
}

func (s *SSH) exec(ctx context.Context, command string, stdout, stderr io.Writer) error {
	if err := s.prepareKey(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "ssh", append(s.sshArgs(), s.ShellCommand(command))...)
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if stdout == nil {
		cmd.Stdout = s.Stdout
	}
	if stderr == nil {
		cmd.Stderr = s.Stderr
	}
	return s.wrap("ssh "+s.destination(), cmd.Run())
}

// Push implements Runner by shelling out to rsync over the shared SSH
// connection.
//
// The trailing slashes are load bearing: `rsync -a src/ host:dir/` copies the
// *contents* of src into dir, which is what an idempotent converge wants. The
// --delete makes the remote tree a mirror, so a file deleted locally is
// removed remotely instead of lingering as a manifest the daemon rediscovers
// on its next restart.
func (s *SSH) Push(ctx context.Context, localDir, remoteDir string, extraArgs ...string) error {
	// The key must exist before the ssh options are assembled: the -i flag is
	// part of them, and rsync runs its own ssh rather than reusing the control
	// master's authentication.
	if err := s.prepareKey(); err != nil {
		return err
	}

	args := []string{
		"-a",
		"--delete",
		"--human-readable",
		"-e", "ssh " + strings.Join(s.sshBaseArgs(), " "),
		// The identity marker names a running instance, so it must neither
		// travel nor be deleted: the destination registers its own identity,
		// and --delete must not remove the marker it already has. An exclude
		// does both, because rsync protects excluded files from --delete unless
		// --delete-excluded is set.
		"--exclude", identity.MarkerFileName,
		"--exclude", identity.MarkerTempPrefix + "*",
	}
	// rsync authenticates as the login user and runs its own remote command, so
	// a non-root login needs the remote side to escalate as well. Without this,
	// pushing into an install root like /opt fails with "mkdir: Permission
	// denied" no matter how well sudo works for scripts.
	if s.needsRemoteSudo() {
		args = append(args, "--rsync-path", "sudo -n rsync")
	}
	args = append(args, extraArgs...)
	args = append(args,
		ensureTrailingSlash(localDir),
		s.destination()+":"+ensureTrailingSlash(remoteDir),
	)

	cmd := exec.CommandContext(ctx, "rsync", args...)
	var stderr bytes.Buffer
	cmd.Stdout = s.Stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return fmt.Errorf("rsync to %s: %w", s.destination(), err)
		}
		return fmt.Errorf("rsync to %s: %w\n%s", s.destination(), err, indent(msg))
	}
	return nil
}

// Close tears down the shared connection and removes temporary files.
func (s *SSH) Close() error {
	if s.masterPath != "" {
		// Best effort: the master also expires on its own after ControlPersist.
		_ = exec.Command("ssh", append(s.sshBaseArgs(),
			"-O", "exit", s.destination())...).Run()
	}
	if s.dir == "" {
		return nil
	}
	err := os.RemoveAll(s.dir)
	s.dir, s.masterPath, s.keyPath = "", "", ""
	s.opened = false
	return err
}

func (s *SSH) wrap(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", what, err)
}

// ShellQuote quotes a string for safe interpolation into a POSIX shell
// command. Single quotes have no special meaning inside single quotes, so the
// standard trick is to close, escape and reopen.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func ensureTrailingSlash(p string) string {
	if strings.HasSuffix(p, "/") {
		return p
	}
	return p + "/"
}

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = "  " + line
	}
	return strings.Join(lines, "\n")
}
