package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/tkoizumi/otter/internal/cloud"
	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/deploy"
	"github.com/tkoizumi/otter/internal/release"
)

// cmdDeployCloud is `otter deploy --cloud`: the self-serve promotion path.
//
// It reuses the release machinery to produce the package and its CANONICAL
// digest, then hands the archive to the control plane. The digest is the
// release's identity, computed by stageJob from the same inputs `otter release`
// hashes; it is never derived from the archive bytes, which are a transport
// checksum that changes whenever the encoder does.
//
// The SSH path is untouched: `otter deploy --host` still converges a host with
// no Cloud in the loop, and the two modes are refused together rather than
// guessing which one was meant.
func (a *App) cmdDeployCloud(ctx context.Context, g globals, f *deploy.Flags, runtimeFlag string) int {
	ident, err := cloud.Resolve("", "")
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}
	if ident.Token == "" {
		fmt.Fprintln(a.Stderr, "otter: not logged in to Otter Cloud")
		fmt.Fprintf(a.Stderr, "otter: run otter login, or set %s\n", cloud.TokenEnv)
		return 1
	}
	client := cloud.NewClient(ident.BaseURL, ident.Token)

	// The organization and its runtimes are read before anything is staged: a
	// deploy that cannot be targeted should fail before it spends time
	// packaging.
	me, err := client.Me(ctx)
	if err != nil {
		return a.cloudFail(err)
	}
	runtime, code := resolveCloudRuntime(a, me, runtimeFlag)
	if code != 0 {
		return code
	}

	// Resolve the workspace and the one job being deployed exactly as
	// `otter release` does, so `--job` names the same thing in both commands.
	jobsRoot, code := resolveJobsRoot(a.Stderr, config.DefaultJobs, false)
	if code != 0 {
		return code
	}
	dataDir, code := resolveWorkspaceData(a.Stderr, "", false)
	if code != 0 {
		return code
	}
	manager := release.Manager{DataDir: dataDir}

	named := strings.TrimSpace(f.Job) != ""
	ref := "."
	if named {
		ref = f.Job
	}
	targets, code := a.jobTargets(ref, jobsRoot, "", named, false)
	if code != 0 {
		return code
	}
	targets, code = a.mapTargetIdentities(ctx, jobsRoot, dataDir, targets)
	if code != 0 {
		return code
	}
	if len(targets) != 1 {
		fmt.Fprintln(a.Stderr, "otter: deploy --cloud stages one job at a time; name one with --job <name>")
		return 2
	}
	target := targets[0]

	// Bound staging the way `otter release` does: a managed environment that
	// hangs on a download must not hold the deploy open indefinitely.
	stageCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	meta, err := a.stageJob(stageCtx, manager, jobsRoot, target, "", prepareOptions{})
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: cloud deploy %s: %v\n", target.Label(), err)
		return 1
	}
	pkgPath, digest, artifact, code := a.writeCloudPackage(manager, target, meta)
	if code != 0 {
		return code
	}
	defer func() { _ = os.Remove(pkgPath) }()

	// Always read the generation, even for a dry run: it is a read, and showing
	// the value a real deploy would present is part of what --dry-run is for.
	state, err := client.Operations(ctx, runtime.ID)
	if err != nil {
		return a.cloudFail(err)
	}

	if f.DryRun {
		fmt.Fprintf(a.Stdout, "would deploy %s to %s (%s)\n", target.Label(), runtime.ID, me.Organization.Name)
		fmt.Fprintf(a.Stdout, "release:       %s\n", digest)
		fmt.Fprintf(a.Stdout, "artifact:      %s\n", artifact)
		fmt.Fprintf(a.Stdout, "generation:    %d\n", state.Generation)
		fmt.Fprintf(a.Stdout, "cloud:         %s\n", ident.BaseURL)
		fmt.Fprintf(a.Stdout, "\nnothing was sent (--dry-run)\n")
		return 0
	}

	pkg, err := os.Open(pkgPath)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: open package: %v\n", err)
		return 1
	}
	_, err = client.UploadRelease(ctx, digest, pkg)
	_ = pkg.Close()
	if err != nil {
		return a.cloudFail(err)
	}

	admission, err := client.Deploy(ctx, runtime.ID, digest, cloud.Operator(), state.Generation)
	if err != nil {
		var refused *cloud.RefusedError
		if errors.As(err, &refused) {
			fmt.Fprintf(a.Stderr, "otter: Cloud refused the deploy: %s\n", refused.Message)
			return 1
		}
		return a.cloudFail(err)
	}

	operationID := "-"
	if admission.Operation != nil && admission.Operation.ID != "" {
		operationID = admission.Operation.ID
	}
	fmt.Fprintf(a.Stdout, "deployed %s to runtime %s\n", target.Label(), runtime.ID)
	fmt.Fprintf(a.Stdout, "operation:     %s\n", operationID)
	fmt.Fprintf(a.Stdout, "release:       %s\n", digest)
	fmt.Fprintf(a.Stdout, "organization:  %s (%s)\n", me.Organization.Name, me.Organization.ID)
	fmt.Fprintf(a.Stdout, "cloud:         %s\n", ident.BaseURL)
	fmt.Fprintf(a.Stdout, "watch:         %s/runtimes/%s\n", ident.BaseURL, runtime.ID)
	return 0
}

// writeCloudPackage serializes the staged release the same way `otter release
// --package` does and returns the temp file plus both identities: the release
// digest that is the deploy target and the archive hash that is the transport
// checksum.
func (a *App) writeCloudPackage(manager release.Manager, target jobTarget, meta release.Metadata) (path, digest, artifact string, code int) {
	dir, err := manager.Dir(target.ID, meta.Digest)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: cloud deploy %s: %v\n", target.Label(), err)
		return "", "", "", 1
	}
	f, err := os.CreateTemp("", "otter-cloud-*.tar.gz")
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: create package: %v\n", err)
		return "", "", "", 1
	}
	packaged, artifact, packErr := release.Package(dir, f)
	closeErr := f.Close()
	if packErr != nil {
		_ = os.Remove(f.Name())
		fmt.Fprintf(a.Stderr, "otter: cloud deploy %s: package: %v\n", target.Label(), packErr)
		return "", "", "", 1
	}
	if closeErr != nil {
		_ = os.Remove(f.Name())
		fmt.Fprintf(a.Stderr, "otter: cloud deploy %s: package: %v\n", target.Label(), closeErr)
		return "", "", "", 1
	}
	// A package that does not carry the identity just staged is not something
	// to publish; this cannot happen if staging is intact, so a mismatch is a
	// refusal rather than a warning.
	if packaged.Digest != meta.Digest {
		_ = os.Remove(f.Name())
		fmt.Fprintf(a.Stderr, "otter: cloud deploy %s: package carries %s, staged release is %s\n",
			target.Label(), packaged.Digest, meta.Digest)
		return "", "", "", 1
	}
	fmt.Fprintf(a.Stdout, "%s: release_digest=%s artifact_sha256=%s\n", target.Label(), packaged.Digest, artifact)
	return f.Name(), packaged.Digest, artifact, 0
}

// resolveCloudRuntime applies the assignment rule: exactly one runtime is used
// without being named, none is a clear refusal, and more than one requires
// --runtime. Guessing among several would promote a release onto a runtime the
// operator did not choose, which is the one wrong answer a deploy must not
// give.
func resolveCloudRuntime(a *App, me *cloud.Me, selected string) (cloud.Runtime, int) {
	selected = strings.TrimSpace(selected)
	if selected != "" {
		for _, rt := range me.Runtimes {
			if rt.ID == selected {
				return rt, 0
			}
		}
		fmt.Fprintf(a.Stderr, "otter: runtime %s is not assigned to organization %s\n", selected, me.Organization.Name)
		writeCloudRuntimeList(a.Stderr, me.Runtimes)
		return cloud.Runtime{}, 2
	}
	switch len(me.Runtimes) {
	case 0:
		fmt.Fprintln(a.Stderr, "otter: no runtime is assigned to this organization yet")
		fmt.Fprintln(a.Stderr, "otter: attach a runtime in Otter Cloud, then re-run this deploy")
		return cloud.Runtime{}, 1
	case 1:
		return me.Runtimes[0], 0
	default:
		fmt.Fprintf(a.Stderr, "otter: this organization has %d runtimes; choose one with --runtime <id>\n", len(me.Runtimes))
		writeCloudRuntimeList(a.Stderr, me.Runtimes)
		return cloud.Runtime{}, 2
	}
}

// writeCloudRuntimeList names the candidate runtimes on stderr, so the error
// that asks for --runtime also supplies the ids to pass.
func writeCloudRuntimeList(w io.Writer, runtimes []cloud.Runtime) {
	if len(runtimes) == 0 {
		return
	}
	fmt.Fprintln(w, "otter: available runtimes:")
	for _, rt := range runtimes {
		details := strings.Trim(strings.Join([]string{rt.Lifecycle, rt.Placement}, ", "), ", ")
		if details != "" {
			fmt.Fprintf(w, "  %s (%s)\n", rt.ID, details)
			continue
		}
		fmt.Fprintf(w, "  %s\n", rt.ID)
	}
}

// cloudFail reports a control-plane failure, adding the one hint a rejected
// credential needs: a 401 is almost always an expired or revoked token, and
// re-running `otter login` is the fix.
func (a *App) cloudFail(err error) int {
	fmt.Fprintf(a.Stderr, "otter: %v\n", err)
	if cloud.IsUnauthorized(err) {
		fmt.Fprintln(a.Stderr, "otter: the Cloud credential was rejected; run otter login again")
	}
	return 1
}
