package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/tkoizumi/otter/internal/cloud"
)

// `otter login` and `otter logout` are the self-serve half of Otter Cloud.
//
// They exist so an operator can reach the hosted control plane without an
// administrator minting a file for them: login verifies a token against
// /api/cloud/me and stores it once, in the operator's own home, and logout
// removes it. The proxy in between -- `otter deploy --cloud` -- then has a
// credential it can find without a flag on every command.
//
// Like every other command this is a thin surface: the protocol and the config
// file live in internal/cloud, which a test drives with an httptest server.

// cmdLogin verifies a Cloud token and stores it, or reports the stored identity
// with --status.
//
// The token is taken from --token, then OTTER_CLOUD_TOKEN, then a terminal
// prompt. The global --token flag is deliberately reused rather than shadowed:
// parseGlobals lifts it out of the argument list before the command runs, so a
// per-command copy would never see it.
func (a *App) cmdLogin(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	cloudURL := fs.String("cloud", "", "Otter Cloud base URL (default "+cloud.DefaultURL+", or "+cloud.URLEnv+")")
	token := fs.String("token", "", "Cloud API token (or "+cloud.TokenEnv+"); prompted on the terminal when omitted")
	status := fs.Bool("status", false, "show the stored identity without changing it")
	fs.Usage = func() {
		fmt.Fprintln(a.Stderr, "Usage: otter login [--cloud <url>] [--token <token>] [--status]")
		fmt.Fprintln(a.Stderr)
		fmt.Fprintln(a.Stderr, "Verifies an Otter Cloud token and stores it for later commands.")
		fmt.Fprintln(a.Stderr, "With no --token, the token is prompted for on the terminal.")
		fmt.Fprintln(a.Stderr)
		fmt.Fprintln(a.Stderr, "Flags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(a.Stderr, "otter: unexpected arguments: %v\n", fs.Args())
		return 2
	}

	if *status {
		return a.cloudStatus()
	}

	stored, err := cloud.LoadConfig()
	if err != nil && !errors.Is(err, cloud.ErrNotLoggedIn) {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}
	base := cloud.ResolveBaseURL(*cloudURL, stored)

	// An explicit token can arrive three ways: the subcommand's own flag (for
	// the single-dash spelling), the global --token parseGlobals lifted out,
	// and the environment. A stored token is NOT a fallback here: `otter login`
	// is the command that establishes a credential, so it must never silently
	// succeed by re-verifying an old one.
	value := firstNonEmpty(*token, g.token, os.Getenv(cloud.TokenEnv))
	if value == "" {
		if !isTerminal(os.Stdin) {
			fmt.Fprintln(a.Stderr, "otter: no Cloud token: pass --token <token> or set "+cloud.TokenEnv)
			return 2
		}
		fmt.Fprint(a.Stderr, "Otter Cloud token: ")
		value, err = cloud.ReadSecret(os.Stdin, a.Stderr)
		if err != nil {
			fmt.Fprintf(a.Stderr, "otter: read token: %v\n", err)
			return 1
		}
	}
	if value == "" {
		fmt.Fprintln(a.Stderr, "otter: no token was entered")
		return 2
	}

	client := cloud.NewClient(base, value)
	me, err := client.Me(ctx)
	if err != nil {
		if cloud.IsUnauthorized(err) {
			fmt.Fprintf(a.Stderr, "otter: the Cloud token was rejected by %s\n", base)
			fmt.Fprintf(a.Stderr, "otter: %v\n", err)
			fmt.Fprintln(a.Stderr, "otter: check the token, or mint a new one in Otter Cloud")
			return 1
		}
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}

	cfg := cloud.Config{
		CloudURL:         base,
		Token:            value,
		OrganizationID:   me.Organization.ID,
		OrganizationName: me.Organization.Name,
		SavedAt:          time.Now().UTC(),
	}
	if err := cloud.SaveConfig(cfg); err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}

	fmt.Fprintf(a.Stdout, "logged in to %s\n", base)
	fmt.Fprintf(a.Stdout, "organization:  %s (%s)\n", me.Organization.Name, me.Organization.ID)
	writeCloudRuntimes(a.Stdout, me.Runtimes)
	if path, err := cloud.ConfigPath(); err == nil {
		fmt.Fprintf(a.Stdout, "config:        %s\n", path)
	}
	return 0
}

// cloudStatus prints the stored identity without touching the network or the
// file, so it answers "who am I logged in as?" even when Cloud is unreachable.
func (a *App) cloudStatus() int {
	cfg, err := cloud.LoadConfig()
	if err != nil {
		if errors.Is(err, cloud.ErrNotLoggedIn) {
			fmt.Fprintln(a.Stderr, "otter: not logged in to Otter Cloud; run otter login")
			return 1
		}
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}
	fmt.Fprintf(a.Stdout, "cloud:         %s\n", cfg.CloudURL)
	fmt.Fprintf(a.Stdout, "organization:  %s (%s)\n", cfg.OrganizationName, cfg.OrganizationID)
	if path, pathErr := cloud.ConfigPath(); pathErr == nil {
		fmt.Fprintf(a.Stdout, "config:        %s\n", path)
	}
	if !cfg.SavedAt.IsZero() {
		fmt.Fprintf(a.Stdout, "saved at:      %s\n", cfg.SavedAt.Local().Format("2006-01-02 15:04:05 MST"))
	}
	return 0
}

// cmdLogout removes the stored credential. It is idempotent: a machine that was
// never logged in reports that and succeeds, because "make sure I am logged
// out" should not fail on the second run.
func (a *App) cmdLogout(ctx context.Context, g globals, args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(a.Stderr, "otter: unexpected arguments: %v\n", args)
		fmt.Fprintln(a.Stderr, "otter: usage: otter logout")
		return 2
	}
	path, pathErr := cloud.ConfigPath()
	removed, err := cloud.RemoveConfig()
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}
	if removed.CloudURL == "" && removed.OrganizationName == "" {
		fmt.Fprintln(a.Stdout, "not logged in to Otter Cloud; nothing to remove")
		return 0
	}
	if pathErr == nil {
		fmt.Fprintf(a.Stdout, "removed %s\n", path)
	}
	if removed.OrganizationName != "" {
		fmt.Fprintf(a.Stdout, "logged out of %s (%s)\n", removed.OrganizationName, removed.CloudURL)
	} else {
		fmt.Fprintf(a.Stdout, "logged out of %s\n", removed.CloudURL)
	}
	return 0
}

// writeCloudRuntimes prints the runtimes an organization owns, or the fact
// that it owns none. The list is short enough that lying it out is clearer than
// a table, and a runtime that exists must be visible because it is what
// `--runtime` names.
func writeCloudRuntimes(w io.Writer, runtimes []cloud.Runtime) {
	if len(runtimes) == 0 {
		fmt.Fprintln(w, "runtimes:      none assigned yet")
		return
	}
	for i, rt := range runtimes {
		label := "runtimes:     "
		if i > 0 {
			label = "              "
		}
		details := strings.Trim(strings.Join([]string{rt.Lifecycle, rt.Placement}, ", "), ", ")
		if details != "" {
			fmt.Fprintf(w, "%s %s (%s)\n", label, rt.ID, details)
			continue
		}
		fmt.Fprintf(w, "%s %s\n", label, rt.ID)
	}
}

// firstNonEmpty returns the first non-blank value, so precedence at a call site
// reads as one expression.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
