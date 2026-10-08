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
// It splits building an artifact from shipping it the way `docker build` and
// `docker push` do:
//
//   - the default, and --release, PROMOTES a release `otter release` already
//     built locally. The store is content-addressed, so the digest is decided
//     before the deploy starts and cannot change on the way to Cloud: sending
//     the artifact is a COPY.
//   - --build keeps the original behaviour: package the job from the workspace,
//     upload, promote. It is the escape hatch for an operator who has not
//     released the job yet, and it is the only mode that can need uv, because
//     only it prepares a managed environment.
//
// The digest is the release's identity, computed by stageJob from the same
// inputs `otter release` hashes; it is never derived from the archive bytes,
// which are a transport checksum that changes whenever the encoder does.
//
// The SSH path is untouched: `otter deploy --host` still converges a host with
// no Cloud in the loop, and the two modes are refused together rather than
// guessing which one was meant.
func (a *App) cmdDeployCloud(ctx context.Context, g globals, f *deploy.Flags, runtimeFlag, releaseRef string) int {
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

	// --build and --release name opposite sources for the artifact, so the
	// combination has no meaning to pick between.
	if f.Build && strings.TrimSpace(releaseRef) != "" {
		fmt.Fprintln(a.Stderr, "otter: --build packages the job from the workspace; it cannot be combined with --release")
		return 2
	}

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

	// digest is the release identity Cloud is asked to activate. pkgPath is the
	// transport archive, and it is empty when this run has nothing to send: a
	// promotion of a release Cloud already holds, or a dry run.
	digest, pkgPath, artifact := "", "", ""
	present := false

	if f.Build {
		meta, err := a.stageJob(stageCtx, manager, jobsRoot, target, "", prepareOptions{})
		if err != nil {
			fmt.Fprintf(a.Stderr, "otter: cloud deploy %s: %v\n", target.Label(), err)
			return 1
		}
		digest = meta.Digest
		// --build keeps the original path exactly: the workspace is packaged
		// and uploaded whether or not Cloud already holds the digest. It is the
		// idempotent store that makes the repeated upload safe.
		pkgPath, artifact, code = a.packageReleaseForCloud(manager, target.ID, target.Label(), digest)
		if code != 0 {
			return code
		}
	} else {
		digest, code = a.selectLocalRelease(manager, target, releaseRef)
		if code != 0 {
			return code
		}
		// The probe is a read, so a dry run makes it too: it is what says
		// whether this promotion has anything to transfer.
		present, err = cloudReleasePresent(stageCtx, client, digest)
		if err != nil {
			return a.cloudFail(err)
		}
	}
	// The closure reads pkgPath when it runs, not when it is registered, so a
	// package created after the dry-run gate is removed just the same.
	defer func() {
		if pkgPath != "" {
			_ = os.Remove(pkgPath)
		}
	}()

	// Always read the generation, even for a dry run: it is a read, and showing
	// the value a real deploy would present is part of what --dry-run is for.
	state, err := client.Operations(ctx, runtime.ID)
	if err != nil {
		return a.cloudFail(err)
	}

	if f.DryRun {
		a.printCloudDryRun(target, runtime, me, ident.BaseURL, digest, artifact, state.Generation, f.Build || !present)
		return 0
	}

	// A selected release that Cloud does not hold has to be serialized now.
	// Nothing above staged or prepared anything to get here: the release
	// already exists, and this only copies it into a transport archive.
	if pkgPath == "" && !present {
		var packCode int
		pkgPath, _, packCode = a.packageReleaseForCloud(manager, target.ID, target.Label(), digest)
		if packCode != 0 {
			return packCode
		}
	}

	if pkgPath == "" {
		fmt.Fprintf(a.Stdout, "%s: release %s is already in Cloud\n", target.Label(), shortDigest(digest))
	} else {
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
		fmt.Fprintf(a.Stdout, "%s: uploaded release %s\n", target.Label(), shortDigest(digest))
	}

	// The job id travels with the digest. A control plane that keeps per-job
	// desired state cannot place a release it cannot attribute, and the job the
	// operator named is the only authority for that.
	admission, err := client.Deploy(ctx, runtime.ID, target.ID, digest, cloud.Operator(), state.Generation)
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

// selectLocalRelease resolves which locally staged release a promotion names.
//
// An empty reference and "latest" are the same request: the newest release for
// the job, as release.Manager.Latest defines it. Anything else has to be a
// digest in one of the two spellings the rest of Otter accepts -- bare hex or
// `sha256:<hex>` -- normalised through the release package's normaliser so a
// deploy and the runtime agree on what the digest names.
//
// Every failure here is exit 2. The command could not name a release to
// promote, which is a problem with the invocation rather than with the
// transport or the admission; the message names the command that fixes it.
func (a *App) selectLocalRelease(manager release.Manager, target jobTarget, ref string) (string, int) {
	releases, err := manager.List(target.ID)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: cloud deploy %s: %v\n", target.Label(), err)
		return "", 1
	}
	if len(releases) == 0 {
		fmt.Fprintf(a.Stderr, "otter: no release for %s; run otter release %s first\n", target.Label(), target.Label())
		return "", 2
	}

	ref = strings.TrimSpace(ref)
	if ref == "" || strings.EqualFold(ref, "latest") {
		latest, ok, err := manager.Latest(target.ID)
		if err != nil {
			fmt.Fprintf(a.Stderr, "otter: cloud deploy %s: %v\n", target.Label(), err)
			return "", 1
		}
		if !ok {
			fmt.Fprintf(a.Stderr, "otter: no release for %s; run otter release %s first\n", target.Label(), target.Label())
			return "", 2
		}
		return latest.Digest, 0
	}

	digest, err := release.NormalizeDigest(ref)
	if err != nil {
		fmt.Fprintf(a.Stderr,
			"otter: --release wants a release digest (sha256:<hex> or bare hex) or \"latest\"; %q is not one\n", ref)
		return "", 2
	}
	for _, rel := range releases {
		if rel.Digest == digest {
			return digest, 0
		}
	}
	fmt.Fprintf(a.Stderr, "otter: %s has no release %s; otter release --list %s\n", target.Label(), digest, target.Label())
	return "", 2
}

// packageReleaseForCloud serializes one release from the local store the way
// `otter release --package` does, and returns the temp file's path plus the
// archive's transport checksum.
//
// It never stages, prepares or activates: the release already exists, and a
// copy cannot change what it is. The digest the package carries is read back out
// of the release and refused unless it is the one this deploy named, so a store
// whose directory and metadata disagree cannot make Cloud promote something
// other than the release the operator chose.
func (a *App) packageReleaseForCloud(manager release.Manager, id, label, digest string) (path, artifact string, code int) {
	dir, err := manager.Dir(id, digest)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: cloud deploy %s: %v\n", label, err)
		return "", "", 1
	}
	f, err := os.CreateTemp("", "otter-cloud-*.tar.gz")
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: create package: %v\n", err)
		return "", "", 1
	}
	packaged, artifact, packErr := release.Package(dir, f)
	closeErr := f.Close()
	if packErr != nil {
		_ = os.Remove(f.Name())
		fmt.Fprintf(a.Stderr, "otter: cloud deploy %s: package: %v\n", label, packErr)
		return "", "", 1
	}
	if closeErr != nil {
		_ = os.Remove(f.Name())
		fmt.Fprintf(a.Stderr, "otter: cloud deploy %s: package: %v\n", label, closeErr)
		return "", "", 1
	}
	// A package that does not carry the identity this deploy named is not
	// something to publish; this cannot happen if the store is intact, so a
	// mismatch is a refusal rather than a warning.
	if packaged.Digest != digest {
		_ = os.Remove(f.Name())
		fmt.Fprintf(a.Stderr, "otter: cloud deploy %s: package carries %s, release is %s\n",
			label, packaged.Digest, digest)
		return "", "", 1
	}
	fmt.Fprintf(a.Stdout, "%s: release_digest=%s artifact_sha256=%s\n", label, packaged.Digest, artifact)
	return f.Name(), artifact, 0
}

// cloudReleasePresent asks the control plane whether it already holds a release.
//
// It is a read, so even a dry run may make it. A 404 is the control plane's
// "no", and the promotion then has to upload. A 405 means this deployment does
// not offer the lookup at all, and the honest reading is "not known to be
// present": uploading is content-addressed and idempotent, so proceeding is
// safe, while failing would refuse a promotion the control plane is perfectly
// able to accept.
func cloudReleasePresent(ctx context.Context, client *cloud.Client, digest string) (bool, error) {
	if _, err := client.Release(ctx, digest); err != nil {
		if cloud.IsNotFound(err) || cloud.IsMethodNotAllowed(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// printCloudDryRun reports what a promotion would do without doing any of it.
//
// It names the job and the release digest, because "which release" is the
// question the split makes the operator answer, and it says whether the
// artifact would have to be sent, because that is the only work a promotion
// adds to Cloud when the release is already there.
func (a *App) printCloudDryRun(target jobTarget, runtime cloud.Runtime, me *cloud.Me, baseURL, digest, artifact string, generation int, uploadNeeded bool) {
	upload := "already in Cloud"
	if uploadNeeded {
		upload = "needed"
	}
	fmt.Fprintf(a.Stdout, "would deploy %s to %s (%s)\n", target.Label(), runtime.ID, me.Organization.Name)
	fmt.Fprintf(a.Stdout, "job:           %s\n", target.Label())
	fmt.Fprintf(a.Stdout, "release:       %s\n", digest)
	if artifact != "" {
		fmt.Fprintf(a.Stdout, "artifact:      %s\n", artifact)
	}
	fmt.Fprintf(a.Stdout, "upload:        %s\n", upload)
	fmt.Fprintf(a.Stdout, "generation:    %d\n", generation)
	fmt.Fprintf(a.Stdout, "cloud:         %s\n", baseURL)
	fmt.Fprintf(a.Stdout, "\nnothing was sent (--dry-run)\n")
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
