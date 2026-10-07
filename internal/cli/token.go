package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/tkoizumi/otter/internal/api"
)

// cmdToken manages the runtime's named, scoped operator credentials.
//
// These exist so a control plane can command a runtime on a customer's behalf
// without holding authority over the customer's data. A `read` token reads jobs,
// runs, output and capture metadata. A `control` token does that and can run,
// cancel, pause, resume and schedule. A `capture` token does everything
// `control` does and can additionally read captured request and response
// bodies. None can read job state, register or delete a job, or change the
// daemon's configuration -- those stay with the admin token (CL-21).
//
// `capture` is deliberately not implied by `control`: reading the client's
// traffic is a separate, explicitly named authority (decisions.md, 2026-10-03).
//
// The token is printed once, at creation. The daemon stores only its hash, so a
// token that is lost is replaced rather than recovered.
func (a *App) cmdToken(ctx context.Context, g globals, args []string) int {
	// The verb is positional and the flags follow it, but Go's flag package
	// stops at the first non-flag argument, so the verb comes off first.
	verb := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		verb, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	name := fs.String("name", "", "a label for the credential, so it can be identified later")
	scope := fs.String("scope", "", "read, control, capture or agent")
	fs.Usage = func() {
		fmt.Fprint(a.Stderr, "Usage: otter token <create|list|revoke> [flags] [id]\n\n"+
			"Manages the runtime's scoped operator credentials.\n\n"+
			"  otter token create --name cloud-gateway --scope control\n"+
			"  otter token create --name runtime-agent --scope agent\n"+
			"  otter token list\n"+
			"  otter token revoke <id>\n\n"+
			"Scopes:\n"+
			"  read     read jobs, runs, output, the timeline and capture metadata\n"+
			"  control  read, plus run, cancel, pause, resume and schedule\n"+
			"  capture  control, plus captured request and response bodies\n"+
			"  agent    the runtime agent's own apply surface only: maintenance,\n"+
			"           active releases, install and activate. No jobs, runs,\n"+
			"           logs, state, config or captures.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	client := g.client()

	switch verb {
	case "create":
		if len(rest) != 0 {
			fmt.Fprint(a.Stderr, "otter: usage: otter token create --name <name> --scope <read|control|capture|agent>\n")
			return 2
		}
		created, err := client.CreateAPIToken(ctx, *name, api.Scope(strings.TrimSpace(*scope)))
		if err != nil {
			return a.fail(err)
		}
		if g.jsonOut {
			return a.printJSON(created)
		}
		fmt.Fprintf(a.Stdout, "%s\n", created.Token)
		fmt.Fprintf(a.Stdout, "name: %s  scope: %s  id: %s\n", created.Name, created.Scope, created.ID)
		fmt.Fprintln(a.Stdout, "This token is shown once and cannot be recovered. Store it now.")
		return 0

	case "list":
		if len(rest) != 0 {
			fmt.Fprint(a.Stderr, "otter: usage: otter token list\n")
			return 2
		}
		list, err := client.ListAPITokens(ctx)
		if err != nil {
			return a.fail(err)
		}
		if g.jsonOut {
			return a.printJSON(list)
		}
		a.printAPITokens(list.Tokens)
		return 0

	case "revoke":
		if len(rest) != 1 {
			fmt.Fprint(a.Stderr, "otter: usage: otter token revoke <id>\n")
			return 2
		}
		if _, err := client.RevokeAPIToken(ctx, rest[0]); err != nil {
			return a.fail(err)
		}
		// Revocation takes effect on the next request the token makes; there is
		// nothing to restart and no cache to wait out.
		fmt.Fprintf(a.Stdout, "revoked %s\n", rest[0])
		return 0

	default:
		fs.Usage()
		return 2
	}
}

// printAPITokens renders the credentials as a table. A revoked token stays in
// the list, with the date it was withdrawn, so an operator can see what used to
// have access rather than only what has it now.
func (a *App) printAPITokens(tokens []api.APITokenView) {
	if len(tokens) == 0 {
		fmt.Fprintln(a.Stdout, "no scoped tokens")
		return
	}

	w := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tSCOPE\tCREATED\tSTATUS")
	for _, t := range tokens {
		status := "active"
		if t.RevokedAt != nil {
			status = "revoked " + t.RevokedAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			t.ID, t.Name, t.Scope, t.CreatedAt.Local().Format("2006-01-02 15:04"), status)
	}
	_ = w.Flush()
}
