// Package pool is the host side of automated tenant provisioning.
//
// A pool host runs one of these. It is the third puller in the system, and it
// exists because creating a runtime cannot be authorized by a runtime credential:
// no runtime exists yet when the work is needed. So the host holds a host-scoped
// pool token, asks the control plane for work, and executes it locally.
//
// WHAT THIS PACKAGE DOES NOT DO. It does not decide policy. The control plane
// decides which runtime, which organization, which subnet and which limits; this
// side decides how to create a network, a volume and a container on *this* machine.
// That split is what keeps the control plane from needing to know about Docker, and
// what lets a different host type be added without touching the scheduler.
//
// IT RUNS THE EXISTING PROVISIONER. `provision-tenant.sh` is 557 lines of tested,
// frozen launch contract — gVisor runtime, uid split, capability drop, per-tenant
// network and resolver, capacity refusal. Reimplementing that in Go would create a
// second copy of an isolation contract, and the two copies would drift. So this
// package renders the provisioner's documented `--config` file, invokes it, and
// then VERIFIES the result by inspecting the container rather than trusting the
// exit status alone.
//
// RECONCILIATION IS THE INTERESTING PART. `provision-tenant.sh` refuses a container
// that already exists, which is correct for a human running a runbook and wrong for
// a retry loop: a report lost to a network blip would turn into a permanent
// failure. So before invoking the provisioner this checks whether the tenant is
// already present and matching the plan, and reports success without re-running
// anything.
package pool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Default paths, matching `pool/host-bootstrap.sh` and `pool/pool-host.sh`.
//
// `ProvisionerPath` is where `pool-host.sh init` installs the pool scripts. The
// scripts are installed rather than assumed to be in a source checkout because a
// pool host is not a development machine: the runbook already warns that
// provisioning must not have to reach back into a repository.
const (
	DefaultProvisionerPath = "/usr/local/lib/otter/pool/provision-tenant.sh"
	DefaultTenantScript    = "/usr/local/lib/otter/pool/tenant.sh"
	DefaultStatePath       = "/var/lib/otter/pool-agent/state.json"
	DefaultVolumeRoot      = "/var/lib/otter/tenants"
	DefaultTokenPath       = "/etc/otter/pool-token"
	DefaultImage           = "otter-runtime:local"
)

// TenantLimits is what the plan asks the container to be allowed.
type TenantLimits struct {
	Memory         string `json:"memory"`
	CPUs           string `json:"cpus"`
	Pids           int    `json:"pids"`
	TmpfsSize      string `json:"tmpfs_size"`
	AgentTmpfsSize string `json:"agent_tmpfs_size"`
}

// Plan is one unit of work: everything needed to create one tenant.
//
// The field names are the control plane's wire names exactly. There is no
// translation layer, because a translation layer is where "memory" and "memory_mb"
// come to mean two different things.
type Plan struct {
	RuntimeID       string       `json:"runtime_id"`
	OrganizationID  string       `json:"organization_id"`
	Tenant          string       `json:"tenant"`
	Subnet          string       `json:"subnet"`
	Image           string       `json:"image"`
	Isolation       string       `json:"isolation"`
	Limits          TenantLimits `json:"limits"`
	BootstrapSecret string       `json:"bootstrap_secret"`
	Attempts        int          `json:"attempts"`
	LeaseExpiresAt  int64        `json:"lease_expires_at"`
}

// WorkResponse is the answer to "is there anything for me?".
type WorkResponse struct {
	Work     *Plan `json:"work"`
	Usage    Usage `json:"usage"`
	Capacity int   `json:"capacity"`
}

// Usage is what the control plane believes this host holds.
type Usage struct {
	Assigned int `json:"assigned"`
	InFlight int `json:"inFlight"`
	Free     int `json:"free"`
}

// Placement is one tenant the host can see on itself.
type Placement struct {
	RuntimeID string `json:"runtime_id"`
	State     string `json:"state"`
	Note      string `json:"note,omitempty"`
}

// Report is the outcome of an attempt plus whatever the host observed.
type Report struct {
	RuntimeID  string      `json:"runtime_id,omitempty"`
	State      string      `json:"state,omitempty"`
	Reason     string      `json:"reason,omitempty"`
	Note       string      `json:"note,omitempty"`
	TenantState string     `json:"tenant_state,omitempty"`
	Placements []Placement `json:"placements,omitempty"`
	FreeDiskMB *int        `json:"free_disk_mb,omitempty"`
	TenantCount *int       `json:"tenant_count,omitempty"`
}

// Options is how the agent is configured. Every field has a default, so the
// systemd unit passes only what it must.
type Options struct {
	CloudURL   string
	TokenPath  string
	StatePath  string
	Provisioner string
	Tenant      string
	// VolumeRoot is where tenant volumes live, for the free-space report. It is the
	// same root the provisioner derives a container's volume from.
	VolumeRoot string
	// PollInterval is how long to wait between asking for work. Provisioning is not
	// latency-sensitive the way a release rollout is, so this is seconds rather than
	// the runtime agent's thirty-second long-poll.
	PollInterval time.Duration
	// Once runs a single cycle and exits, for a drill or a systemd oneshot.
	Once bool
	// DryRun prints what would be executed and changes nothing.
	DryRun bool
	// CommandRunner is injectable so the executor is testable without Docker.
	CommandRunner Runner
	// Clock is injectable so a test does not sleep.
	Clock  func() time.Time
	Logger func(format string, args ...any)
}

func (o *Options) withDefaults() Options {
	out := *o
	if out.CloudURL == "" {
		out.CloudURL = os.Getenv("OTTER_POOL_CLOUD_URL")
	}
	if out.TokenPath == "" {
		out.TokenPath = firstNonEmpty(os.Getenv("OTTER_POOL_TOKEN_FILE"), DefaultTokenPath)
	}
	if out.StatePath == "" {
		out.StatePath = firstNonEmpty(os.Getenv("OTTER_POOL_STATE"), DefaultStatePath)
	}
	if out.Provisioner == "" {
		out.Provisioner = firstNonEmpty(os.Getenv("OTTER_POOL_PROVISIONER"), DefaultProvisionerPath)
	}
	if out.Tenant == "" {
		out.Tenant = firstNonEmpty(os.Getenv("OTTER_POOL_TENANT_SCRIPT"), DefaultTenantScript)
	}
	if out.VolumeRoot == "" {
		out.VolumeRoot = firstNonEmpty(os.Getenv("OTTER_TENANTS_DIR"), DefaultVolumeRoot)
	}
	if out.PollInterval <= 0 {
		out.PollInterval = 15 * time.Second
	}
	if out.Clock == nil {
		out.Clock = time.Now
	}
	if out.Logger == nil {
		out.Logger = func(string, ...any) {}
	}
	if out.CommandRunner == nil {
		out.CommandRunner = ExecRunner{}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// TenantConfigPath is where the rendered provisioner config is written.
//
// Beside the state file rather than in `/tmp`: the file names a subnet and an
// organization and is what an operator reads to understand what the agent did, so
// it should survive a reboot. It holds no secret.
func TenantConfigPath(statePath, tenant string) string {
	return filepath.Join(filepath.Dir(statePath), "tenants", tenant+".conf")
}

// RenderConfig builds the `--config` file `provision-tenant.sh` documents.
//
// The key names are the provisioner's own (`provision-tenant.sh:270-295`), including
// both spellings it accepts. Quote-free `key=value` is deliberate: the loader reads
// lines and splits on the first `=`, so a value containing an `=` would be taken
// whole, and a value containing a newline would break the file — which is why the
// secret is passed as a FILE rather than in this config.
func RenderConfig(plan Plan, cloudURL string) string {
	var b strings.Builder
	write := func(key, value string) {
		fmt.Fprintf(&b, "%s=%s\n", key, value)
	}
	write("name", plan.Tenant)
	write("runtime_id", plan.RuntimeID)
	write("org", plan.OrganizationID)
	write("subnet", plan.Subnet)
	write("cloud_url", cloudURL)
	write("image", firstNonEmpty(plan.Image, DefaultImage))
	limits := plan.Limits
	write("memory", firstNonEmpty(limits.Memory, "320m"))
	write("cpus", firstNonEmpty(limits.CPUs, "1"))
	write("pids", strconv.Itoa(orDefault(limits.Pids, 64)))
	write("tmpfs_size", firstNonEmpty(limits.TmpfsSize, "2g"))
	write("agent_tmpfs_size", firstNonEmpty(limits.AgentTmpfsSize, "64m"))
	return b.String()
}

func orDefault(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

// ContainerName is the container a tenant runs in, derived the way the provisioner
// derives it (`provision-tenant.sh:437`). One name is the whole identity: container,
// network and volume all follow from it.
func ContainerName(tenant string) string { return "otter-" + tenant }

// Outcome is what an execution attempt produced.
type Outcome struct {
	// AlreadyPresent is true when reconciliation found the tenant in place, so the
	// provisioner was not invoked at all.
	AlreadyPresent bool
	// Verified is true when the container was inspected after the run and matches the
	// plan: right runtime id, right tenant, running.
	Verified bool
	// Detail is a short human description for the operator's log and the report note.
	Detail string
}

// Execute provisions one tenant, or verifies that it is already provisioned.
//
// The order is: reconcile, then act, then verify. Verification is not decorative —
// it is what makes a report trustworthy, because "the script exited zero" and
// "a container with this runtime id is running" are different claims and only the
// second one justifies telling a user their runtime exists.
func Execute(ctx context.Context, plan Plan, opts Options) (Outcome, error) {
	o := opts.withDefaults()

	// Reconcile first: an existing matching container is success, and re-running the
	// provisioner against one is a hard refusal by design.
	observed, err := InspectTenant(ctx, plan.Tenant, o)
	if err != nil {
		// An inspection failure must not be read as "absent": acting on that would
		// re-run a provisioner against a container that may exist.
		return Outcome{}, fmt.Errorf("inspect %s: %w", ContainerName(plan.Tenant), err)
	}
	if observed.Present {
		if !observed.MatchesPlan(plan) {
			return Outcome{}, fmt.Errorf(
				"container %s exists but does not match this plan (runtime-id %q, want %q); refusing to touch it",
				ContainerName(plan.Tenant), observed.RuntimeID, plan.RuntimeID,
			)
		}
		if observed.Running {
			return Outcome{AlreadyPresent: true, Verified: true, Detail: "already provisioned and running"}, nil
		}
		// Present but stopped: start it rather than recreating it. Recreating would
		// destroy the tenant's workspace, which is the one thing that must survive.
		if _, err := o.CommandRunner.Run(ctx, "docker", "start", ContainerName(plan.Tenant)); err != nil {
			return Outcome{}, fmt.Errorf("start %s: %w", ContainerName(plan.Tenant), err)
		}
		after, err := InspectTenant(ctx, plan.Tenant, o)
		if err != nil {
			return Outcome{}, fmt.Errorf("inspect %s after start: %w", ContainerName(plan.Tenant), err)
		}
		if !after.Running {
			return Outcome{}, fmt.Errorf("container %s did not reach a running state", ContainerName(plan.Tenant))
		}
		return Outcome{AlreadyPresent: true, Verified: true, Detail: "already provisioned; started a stopped container"}, nil
	}

	// The secret goes in a 0600 file, never on the command line: an argv is visible
	// to every process on the host, and this credential can claim a tenant.
	secretPath, cleanup, err := writeSecret(plan.BootstrapSecret)
	if err != nil {
		return Outcome{}, err
	}
	defer cleanup()

	configPath := TenantConfigPath(o.StatePath, plan.Tenant)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return Outcome{}, fmt.Errorf("prepare %s: %w", filepath.Dir(configPath), err)
	}
	if err := os.WriteFile(configPath, []byte(RenderConfig(plan, o.CloudURL)), 0o644); err != nil {
		return Outcome{}, fmt.Errorf("write %s: %w", configPath, err)
	}

	if o.DryRun {
		return Outcome{Detail: "dry run: provisioner not invoked"}, nil
	}

	// `sh <script> --config` rather than executing the script directly: the pool
	// scripts are installed mode 0755 by `pool-host.sh`, but an operator who copied
	// them without the bit would otherwise get a confusing EACCES, and `sh` also
	// means this works from a checkout with no packaging step.
	out, err := o.CommandRunner.Run(ctx, "sh", o.Provisioner, "--config", configPath, "--bootstrap-secret-file", secretPath)
	if err != nil {
		// The provisioner refuses an existing container (`provision-tenant.sh:552`).
		// A retry can race a slow first attempt, so re-inspect before believing the
		// failure: if a matching container is there now, the goal state was reached.
		if again, inspectErr := InspectTenant(ctx, plan.Tenant, o); inspectErr == nil && again.Present && again.MatchesPlan(plan) && again.Running {
			return Outcome{AlreadyPresent: true, Verified: true, Detail: "provisioner reported an existing container; it matches the plan and is running"}, nil
		}
		return Outcome{}, fmt.Errorf("provision %s: %w: %s", plan.Tenant, err, trimOutput(out))
	}

	after, err := InspectTenant(ctx, plan.Tenant, o)
	if err != nil {
		return Outcome{}, fmt.Errorf("inspect %s after provisioning: %w", ContainerName(plan.Tenant), err)
	}
	if !after.Present {
		return Outcome{}, fmt.Errorf("provisioner exited successfully but container %s does not exist", ContainerName(plan.Tenant))
	}
	if !after.MatchesPlan(plan) {
		return Outcome{}, fmt.Errorf(
			"container %s carries runtime-id %q, want %q",
			ContainerName(plan.Tenant), after.RuntimeID, plan.RuntimeID,
		)
	}
	if !after.Running {
		return Outcome{}, fmt.Errorf("container %s exists but is not running", ContainerName(plan.Tenant))
	}
	return Outcome{Verified: true, Detail: "provisioned and verified"}, nil
}

// writeSecret stores a secret in a 0600 file and returns its path plus a cleanup.
//
// The file is removed even on failure, and it is deliberately NOT written into the
// agent's state directory: a secret that outlives the attempt that needed it is a
// liability, and the provisioner only reads it once.
func writeSecret(secret string) (string, func(), error) {
	if strings.TrimSpace(secret) == "" {
		return "", func() {}, fmt.Errorf("the plan carries no bootstrap secret, so the tenant's agent could never authenticate")
	}
	file, err := os.CreateTemp("", "otter-pool-secret-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create secret file: %w", err)
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("chmod secret file: %w", err)
	}
	if _, err := file.WriteString(secret); err != nil {
		file.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("write secret file: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("close secret file: %w", err)
	}
	return path, cleanup, nil
}

// trimOutput bounds a command's output for an error message. A provisioner failure
// can print hundreds of lines; the report carries the tail, which is where the
// reason is.
func trimOutput(out string) string {
	trimmed := strings.TrimSpace(out)
	const limit = 600
	if len(trimmed) <= limit {
		return trimmed
	}
	return "…" + trimmed[len(trimmed)-limit:]
}
