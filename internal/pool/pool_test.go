package pool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The executor is where a retry loop meets a provisioner that deliberately refuses an
// existing container. These tests are about that boundary: reconciliation decides
// whether the provisioner is invoked at all, and verification decides whether the
// control plane is told a runtime exists.

func testOptions(t *testing.T, runner Runner) Options {
	t.Helper()
	return Options{
		CloudURL:      "https://app.runotter.dev",
		StatePath:     filepath.Join(t.TempDir(), "state.json"),
		Provisioner:   "/usr/local/lib/otter/pool/provision-tenant.sh",
		CommandRunner: runner,
	}
}

func plan() Plan {
	return Plan{
		RuntimeID:       "rt_9f3a1c2b4d5e",
		OrganizationID:  "org-1",
		Tenant:          "rt9f3a1c2b4d5e",
		Subnet:          "100.64.0.0/24",
		Image:           "otter-runtime:local",
		Isolation:       "runsc-v1",
		BootstrapSecret: "s3cret",
		Limits:          TenantLimits{Memory: "320m", CPUs: "1", Pids: 64, TmpfsSize: "2g", AgentTmpfsSize: "64m"},
	}
}

// inspectAnswer scripts `docker inspect` for one tenant.
func inspectAnswer(present, running bool, runtimeID string) func(string, []string) (string, error) {
	return func(name string, args []string) (string, error) {
		if name != "docker" || len(args) == 0 || args[0] != "inspect" {
			return "", nil
		}
		if !present {
			return "Error: No such object: otter-x", errors.New("exit status 1")
		}
		state := "false"
		status := "exited"
		if running {
			state, status = "true", "running"
		}
		return strings.Join([]string{state, status, runtimeID, "org-1", "runtime"}, "|"), nil
	}
}

func TestRenderConfigUsesTheProvisionersOwnKeys(t *testing.T) {
	config := RenderConfig(plan(), "https://app.runotter.dev")

	for _, want := range []string{
		"name=rt9f3a1c2b4d5e",
		"runtime_id=rt_9f3a1c2b4d5e",
		"org=org-1",
		"subnet=100.64.0.0/24",
		"cloud_url=https://app.runotter.dev",
		"image=otter-runtime:local",
		"memory=320m",
		"cpus=1",
		"pids=64",
		"tmpfs_size=2g",
		"agent_tmpfs_size=64m",
	} {
		if !strings.Contains(config, want+"\n") {
			t.Errorf("config is missing %q:\n%s", want, config)
		}
	}
	// The secret must never be in this file: it is passed as a 0600 file the
	// provisioner reads once, and a config that outlives the attempt must not carry it.
	if strings.Contains(config, "s3cret") {
		t.Error("the rendered config must not contain the bootstrap secret")
	}
}

func TestRenderConfigSuppliesDefaultsForAnEmptyPlan(t *testing.T) {
	config := RenderConfig(Plan{Tenant: "rtx", RuntimeID: "rt_x", OrganizationID: "org", Subnet: "100.64.1.0/24"}, "https://cloud")
	for _, want := range []string{"image=otter-runtime:local", "memory=320m", "cpus=1", "pids=64", "tmpfs_size=2g"} {
		if !strings.Contains(config, want+"\n") {
			t.Errorf("defaults must fill %q:\n%s", want, config)
		}
	}
}

func TestExecuteProvisionsAnAbsentTenantThenVerifiesIt(t *testing.T) {
	state := t.TempDir()
	runner := &RecordingRunner{}
	// First inspect: absent. Then the provisioner runs. Second inspect: present and
	// running, which is what makes the report trustworthy.
	calls := 0
	runner.Respond = func(name string, args []string) (string, error) {
		if name == "docker" && len(args) > 0 && args[0] == "inspect" {
			calls++
			if calls == 1 {
				return inspectAnswer(false, false, "")(name, args)
			}
			return inspectAnswer(true, true, "rt_9f3a1c2b4d5e")(name, args)
		}
		return "", nil
	}
	opts := testOptions(t, runner)
	opts.StatePath = filepath.Join(state, "state.json")

	outcome, err := Execute(context.Background(), plan(), opts)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !outcome.Verified || outcome.AlreadyPresent {
		t.Fatalf("want a verified fresh provision, got %+v", outcome)
	}

	// The provisioner ran once, with the config and the secret file.
	var provisioned bool
	for _, call := range runner.Calls {
		if len(call) >= 2 && call[1] == opts.Provisioner {
			provisioned = true
			joined := strings.Join(call, " ")
			if !strings.Contains(joined, "--config") || !strings.Contains(joined, "--bootstrap-secret-file") {
				t.Errorf("the provisioner must be given both the config and the secret file: %s", joined)
			}
		}
	}
	if !provisioned {
		t.Fatalf("the provisioner was never invoked: %v", runner.Calls)
	}

	// The config was written where an operator can read it, and the secret file was
	// removed: it must not outlive the attempt.
	if _, err := os.Stat(TenantConfigPath(opts.StatePath, plan().Tenant)); err != nil {
		t.Errorf("the config should be kept for the operator: %v", err)
	}
	for _, call := range runner.Calls {
		for i, arg := range call {
			if arg == "--bootstrap-secret-file" && i+1 < len(call) {
				if _, err := os.Stat(call[i+1]); !os.IsNotExist(err) {
					t.Errorf("the secret file %s must be removed after the attempt", call[i+1])
				}
			}
		}
	}
}

func TestExecuteReconcilesAnAlreadyProvisionedTenant(t *testing.T) {
	runner := &RecordingRunner{Respond: inspectAnswer(true, true, "rt_9f3a1c2b4d5e")}
	opts := testOptions(t, runner)

	outcome, err := Execute(context.Background(), plan(), opts)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !outcome.AlreadyPresent || !outcome.Verified {
		t.Fatalf("an existing matching container is success, got %+v", outcome)
	}
	// The provisioner must NOT have run: it refuses an existing container by design.
	for _, call := range runner.Calls {
		if len(call) >= 2 && call[1] == opts.Provisioner {
			t.Fatalf("the provisioner must not run against an existing container: %v", call)
		}
	}
}

func TestExecuteStartsAStoppedTenantRatherThanRecreatingIt(t *testing.T) {
	// Recreating would destroy the workspace, which is the one thing that must
	// survive. A stopped container is started.
	calls := 0
	runner := &RecordingRunner{}
	runner.Respond = func(name string, args []string) (string, error) {
		if name == "docker" && len(args) > 0 && args[0] == "inspect" {
			calls++
			if calls == 1 {
				return inspectAnswer(true, false, "rt_9f3a1c2b4d5e")(name, args)
			}
			return inspectAnswer(true, true, "rt_9f3a1c2b4d5e")(name, args)
		}
		return "", nil
	}
	opts := testOptions(t, runner)

	outcome, err := Execute(context.Background(), plan(), opts)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !outcome.AlreadyPresent {
		t.Fatalf("a stopped tenant is already provisioned, got %+v", outcome)
	}
	var started bool
	for _, call := range runner.Calls {
		if len(call) >= 3 && call[0] == "docker" && call[1] == "start" && call[2] == "otter-rt9f3a1c2b4d5e" {
			started = true
		}
	}
	if !started {
		t.Fatalf("the stopped container must be started: %v", runner.Calls)
	}
}

func TestExecuteRefusesAContainerThatIsNotThisPlan(t *testing.T) {
	// A container carrying a different runtime id is a different tenant's, or a
	// leftover. Acting on it would report one workspace's runtime as another's.
	runner := &RecordingRunner{Respond: inspectAnswer(true, true, "rt_someone_else")}
	opts := testOptions(t, runner)

	_, err := Execute(context.Background(), plan(), opts)
	if err == nil {
		t.Fatal("a mismatched container must be refused")
	}
	if !strings.Contains(err.Error(), "does not match this plan") {
		t.Fatalf("the refusal must name the mismatch, got: %v", err)
	}
}

func TestExecuteRefusesAPlanWithNoSecret(t *testing.T) {
	// Without a secret the tenant's agent could never authenticate, so provisioning
	// it would produce a runtime that can never work.
	runner := &RecordingRunner{Respond: inspectAnswer(false, false, "")}
	opts := testOptions(t, runner)
	plan := plan()
	plan.BootstrapSecret = ""

	_, err := Execute(context.Background(), plan, opts)
	if err == nil || !strings.Contains(err.Error(), "no bootstrap secret") {
		t.Fatalf("want a refusal naming the missing secret, got %v", err)
	}
}

func TestExecuteReportsAProvisionerFailureWithItsOutput(t *testing.T) {
	runner := &RecordingRunner{}
	runner.Respond = func(name string, args []string) (string, error) {
		if name == "docker" && len(args) > 0 && args[0] == "inspect" {
			return inspectAnswer(false, false, "")(name, args)
		}
		return "provision-tenant: pool is full (5/5): refusing to provision x", errors.New("exit status 1")
	}
	opts := testOptions(t, runner)

	_, err := Execute(context.Background(), plan(), opts)
	if err == nil {
		t.Fatal("a provisioner failure must be an error")
	}
	if !strings.Contains(err.Error(), "pool is full") {
		t.Fatalf("the reason must survive into the error, got: %v", err)
	}
}

func TestExecuteBelievesAReinspectionOverAProvisionerFailure(t *testing.T) {
	// The retry that races a slow first attempt: the provisioner refuses because the
	// container now exists, and the goal state has in fact been reached.
	calls := 0
	runner := &RecordingRunner{}
	runner.Respond = func(name string, args []string) (string, error) {
		if name == "docker" && len(args) > 0 && args[0] == "inspect" {
			calls++
			if calls == 1 {
				return inspectAnswer(false, false, "")(name, args)
			}
			return inspectAnswer(true, true, "rt_9f3a1c2b4d5e")(name, args)
		}
		return "Error: container otter-rt9f3a1c2b4d5e already exists", errors.New("exit status 1")
	}
	opts := testOptions(t, runner)

	outcome, err := Execute(context.Background(), plan(), opts)
	if err != nil {
		t.Fatalf("a matching running container means the goal state was reached: %v", err)
	}
	if !outcome.AlreadyPresent || !outcome.Verified {
		t.Fatalf("want reconciled success, got %+v", outcome)
	}
}

func TestExecuteDoesNotTreatAnInspectionFailureAsAbsent(t *testing.T) {
	// "docker is not running" must not be read as "nothing is here, safe to create".
	runner := &RecordingRunner{}
	runner.Respond = func(name string, args []string) (string, error) {
		if name == "docker" && len(args) > 0 && args[0] == "inspect" {
			return "Cannot connect to the Docker daemon", errors.New("exit status 1")
		}
		return "", nil
	}
	opts := testOptions(t, runner)

	_, err := Execute(context.Background(), plan(), opts)
	if err == nil {
		t.Fatal("an unreadable docker state must stop the attempt, not provoke a create")
	}
	for _, call := range runner.Calls {
		if len(call) >= 2 && call[1] == opts.Provisioner {
			t.Fatal("the provisioner must not run when the current state is unknown")
		}
	}
}

func TestInspectTenantDistinguishesAbsentFromUnreadable(t *testing.T) {
	absent := &RecordingRunner{Respond: func(string, []string) (string, error) {
		return "Error: No such object: otter-x", errors.New("exit status 1")
	}}
	observed, err := InspectTenant(context.Background(), "x", testOptions(t, absent))
	if err != nil {
		t.Fatalf("a missing container is not an error: %v", err)
	}
	if observed.Present || observed.State != "absent" {
		t.Fatalf("want absent, got %+v", observed)
	}

	broken := &RecordingRunner{Respond: func(string, []string) (string, error) {
		return "Cannot connect to the Docker daemon", errors.New("exit status 1")
	}}
	if _, err := InspectTenant(context.Background(), "x", testOptions(t, broken)); err == nil {
		t.Fatal("an unreadable docker must be an error")
	}
}

func TestListTenantsReadsOnlyRuntimeContainers(t *testing.T) {
	runner := &RecordingRunner{Respond: func(name string, args []string) (string, error) {
		return "otter-rt9f3a1c2b4d5e|running|rt_9f3a1c2b4d5e\notter-pool1|exited|rt-pool-1\n", nil
	}}
	tenants, err := ListTenants(context.Background(), testOptions(t, runner))
	if err != nil {
		t.Fatalf("ListTenants: %v", err)
	}
	if len(tenants) != 2 {
		t.Fatalf("want 2 tenants, got %d", len(tenants))
	}
	if tenants[0].State != "running" || tenants[1].State != "stopped" {
		t.Fatalf("states must be normalised, got %+v", tenants)
	}
	// The filter is part of the contract: a host must not report unrelated
	// containers as tenants.
	joined := Command(runner.Calls[0])
	if !strings.Contains(joined, "label=otter.role=runtime") {
		t.Fatalf("the list must be filtered to tenant runtimes: %s", joined)
	}
}
