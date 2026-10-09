package pool

import (
	"context"
	"fmt"
	"strings"
)

// Observed is what a tenant's container actually looks like on this host.
//
// Every field comes from inspecting the container rather than from anything the
// agent remembers: a host restored from a backup, a container removed by hand, or an
// agent restarted with no state must all be able to answer "what is here?" from the
// machine itself.
type Observed struct {
	Tenant    string
	Present   bool
	Running   bool
	RuntimeID string
	Org       string
	Role      string
	// Status is the container's own status text, for a report note.
	Status string
	// State is the normalised state the control plane understands.
	State string
}

// MatchesPlan reports whether this container is the one the plan describes.
//
// The runtime id is compared because it is the logical identity, and a container
// whose label disagrees is a different tenant's or a leftover from a different
// attempt. Acting on that mismatch would report one workspace's runtime as
// another's.
func (o Observed) MatchesPlan(plan Plan) bool {
	if !o.Present {
		return false
	}
	if o.RuntimeID != "" && o.RuntimeID != plan.RuntimeID {
		return false
	}
	return true
}

// InspectTenant reads the container's state and labels.
//
// A container that does not exist is NOT an error: it is the normal state before the
// first provision, and reporting it as one would make "nothing here yet" look like a
// broken host.
func InspectTenant(ctx context.Context, tenant string, opts Options) (Observed, error) {
	o := opts.withDefaults()
	name := ContainerName(tenant)
	observed := Observed{Tenant: tenant, State: "absent"}

	out, err := o.CommandRunner.Run(ctx, "docker", "inspect",
		"--format", "{{.State.Running}}|{{.State.Status}}|{{index .Config.Labels \"otter.runtime-id\"}}|{{index .Config.Labels \"otter.org\"}}|{{index .Config.Labels \"otter.role\"}}",
		name)
	if err != nil {
		// docker inspect exits non-zero for a missing container. Distinguishing that
		// from a real failure (no docker, permission denied) matters: the first is
		// "absent", the second must not be read as "safe to create".
		if isMissingContainer(err, out) {
			return observed, nil
		}
		return observed, fmt.Errorf("docker inspect %s: %w: %s", name, err, trimOutput(out))
	}

	fields := strings.Split(strings.TrimSpace(out), "|")
	if len(fields) < 5 {
		return observed, fmt.Errorf("docker inspect %s returned an unexpected shape: %q", name, trimOutput(out))
	}
	observed.Present = true
	observed.Running = strings.TrimSpace(fields[0]) == "true"
	observed.Status = strings.TrimSpace(fields[1])
	observed.RuntimeID = strings.TrimSpace(fields[2])
	observed.Org = strings.TrimSpace(fields[3])
	observed.Role = strings.TrimSpace(fields[4])
	observed.State = stateOf(observed.Running, observed.Status)
	return observed, nil
}

// stateOf normalises Docker's status text into the control plane's vocabulary.
//
// `unknown` rather than a guess for anything unrecognised: the control plane treats
// an unrecognised state as an observation it cannot act on, which is the safe
// direction for a runtime a user is waiting on.
func stateOf(running bool, status string) string {
	switch {
	case running:
		return "running"
	case status == "exited" || status == "dead" || status == "created":
		return "stopped"
	case status == "":
		return "unknown"
	default:
		return "stopped"
	}
}

// isMissingContainer distinguishes "no such container" from a real docker failure.
//
// The message shape is Docker's own, and both the CLI's stderr text and its exit
// code are checked because a container name that Docker rejects outright produces a
// different message than one that simply is not there.
func isMissingContainer(err error, out string) bool {
	text := strings.ToLower(out)
	if strings.Contains(text, "no such container") || strings.Contains(text, "no such object") {
		return true
	}
	if strings.Contains(text, "error: no such") {
		return true
	}
	// A non-zero exit with no output at all is not evidence of absence.
	if err != nil && strings.TrimSpace(out) == "" {
		return false
	}
	return false
}

// ListTenants returns every `otter-<name>` container this host can see.
//
// Used for the observation half of a report. It reads Docker's own list rather than
// the control plane's belief, which is the point: the two are compared, and a
// difference is drift an operator should see rather than something the agent
// silently resolves.
func ListTenants(ctx context.Context, opts Options) ([]Observed, error) {
	o := opts.withDefaults()
	out, err := o.CommandRunner.Run(ctx, "docker", "ps", "-a",
		"--filter", "label=otter.role=runtime",
		"--format", "{{.Names}}|{{.State}}|{{.Label \"otter.runtime-id\"}}")
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w: %s", err, trimOutput(out))
	}
	var tenants []Observed
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) < 3 {
			continue
		}
		name := strings.TrimSpace(fields[0])
		observed := Observed{
			Tenant:    strings.TrimPrefix(name, "otter-"),
			Present:   true,
			Running:   strings.TrimSpace(fields[1]) == "running",
			RuntimeID: strings.TrimSpace(fields[2]),
		}
		observed.State = stateOf(observed.Running, strings.TrimSpace(fields[1]))
		tenants = append(tenants, observed)
	}
	return tenants, nil
}

// FreeDiskMB reports free space on the tenant volume root, or nil when it cannot be
// read.
//
// nil rather than zero: a host that cannot measure its disk must say "unknown", and
// reporting zero free would look like a full disk and stop placement for the wrong
// reason. The path is the one the provisioner uses for volumes
// (`provision-tenant.sh:443`).
func FreeDiskMB(ctx context.Context, opts Options) *int {
	o := opts.withDefaults()
	root := o.VolumeRoot
	out, err := o.CommandRunner.Run(ctx, "df", "-Pm", root)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return nil
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return nil
	}
	value, err := parsePositiveInt(fields[3])
	if err != nil {
		return nil
	}
	return &value
}

func parsePositiveInt(value string) (int, error) {
	var out int
	if _, err := fmt.Sscanf(value, "%d", &out); err != nil {
		return 0, err
	}
	return out, nil
}
