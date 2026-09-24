package deploy

import (
	"strings"
	"testing"
	"time"
)

// A workspace name has to be stable across renames and safe in a directory, a
// unit and an environment file name. The slug is cosmetic; the id is identity.
func TestWorkspaceNameIsStableAndSafe(t *testing.T) {
	target := Target{
		RemoteDir:     "/opt/otter",
		WorkspaceID:   "24856da9-1111-2222-3333-444444444444",
		WorkspaceSlug: "My Shopify Exports!",
	}
	if got, want := target.WorkspaceName(), "my-shopify-exports-24856da9"; got != want {
		t.Errorf("WorkspaceName = %q, want %q", got, want)
	}

	// Renaming the project must not move the host tree: the recorded name wins.
	target.WorkspaceSlug = "something-else"
	target.WorkspaceNameOverride = "my-shopify-exports-24856da9"
	if got, want := target.WorkspaceName(), "my-shopify-exports-24856da9"; got != want {
		t.Errorf("WorkspaceName with an override = %q, want %q", got, want)
	}

	// Every path derives from that name.
	if got, want := target.WorkspaceDir(), "/opt/otter/workspaces/my-shopify-exports-24856da9"; got != want {
		t.Errorf("WorkspaceDir = %q, want %q", got, want)
	}
	if got, want := target.BinaryPath(), "/opt/otter/workspaces/my-shopify-exports-24856da9/bin/otterd"; got != want {
		t.Errorf("BinaryPath = %q, want %q", got, want)
	}
	if got, want := target.DataDir, ""; got != want {
		t.Errorf("DataDir = %q before defaults; want empty", got)
	}
	filled := target.fillWorkspaceDefaults()
	if got, want := filled.DataDir, "/opt/otter/workspaces/my-shopify-exports-24856da9/.otter/data"; got != want {
		t.Errorf("DataDir = %q, want %q", got, want)
	}
	if got, want := filled.ServiceName, "otterd-my-shopify-exports-24856da9"; got != want {
		t.Errorf("ServiceName = %q, want %q", got, want)
	}
}

// A port is claimed by a workspace even while its daemon is stopped, and a port
// in use by anything at all must be skipped: handing a workspace a busy port
// fails later as an unexplained unhealthy daemon.
func TestNextListenPortSkipsClaimedAndListeningPorts(t *testing.T) {
	records := []WorkspaceRecord{
		{Name: "one", Listen: "127.0.0.1:7337"},
		{Name: "two", Listen: "127.0.0.1:7339"},
	}
	// 7338 belongs to something that is not a workspace.
	listening := []int{7338, 7400}

	if got, want := NextListenPort(records, listening), 7340; got != want {
		t.Errorf("NextListenPort = %d, want %d", got, want)
	}
	if got, want := NextListenPort(nil, nil), DefaultListenPort; got != want {
		t.Errorf("first port = %d, want %d", got, want)
	}
}

// The same project must land on the same workspace however it is reached; a
// different one must get its own directory and its own port.
func TestSelectWorkspaceAdoptsOrCreates(t *testing.T) {
	records := []WorkspaceRecord{
		{ID: "aaaa1111-0000-0000-0000-000000000000", Slug: "shop", Name: "shop-aaaa1111", Unit: "otterd-shop-aaaa1111", Listen: "127.0.0.1:7337"},
	}

	t.Run("the recorded workspace is adopted by id", func(t *testing.T) {
		target := Target{RemoteDir: "/opt/otter", Host: "h", WorkspaceID: "aaaa1111-0000-0000-0000-000000000000", WorkspaceSlug: "renamed-locally"}
		got, created, err := SelectWorkspace(target, records, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if created {
			t.Error("an existing workspace was reported as created")
		}
		if got.WorkspaceName() != "shop-aaaa1111" {
			t.Errorf("WorkspaceName = %q, want the recorded name", got.WorkspaceName())
		}
		if got.Listen != "127.0.0.1:7337" {
			t.Errorf("Listen = %q, want the recorded port", got.Listen)
		}
		if got.ServiceName != "otterd-shop-aaaa1111" {
			t.Errorf("ServiceName = %q, want the recorded unit", got.ServiceName)
		}
	})

	t.Run("another project gets the next port", func(t *testing.T) {
		target := Target{RemoteDir: "/opt/otter", Host: "h", WorkspaceID: "bbbb2222-0000-0000-0000-000000000000", WorkspaceSlug: "other"}
		got, created, err := SelectWorkspace(target, records, []int{7338}, "")
		if err != nil {
			t.Fatal(err)
		}
		if !created {
			t.Error("a new workspace was not reported as created")
		}
		if got.Listen != "127.0.0.1:7339" {
			t.Errorf("Listen = %q, want 127.0.0.1:7339 (7337 claimed, 7338 busy)", got.Listen)
		}
		if got.WorkspaceDir() == "/opt/otter/workspaces/shop-aaaa1111" {
			t.Error("the new workspace reused the existing directory")
		}
	})

	t.Run("--workspace adopts an existing workspace by name", func(t *testing.T) {
		target := Target{RemoteDir: "/opt/otter", Host: "h", WorkspaceID: "cccc3333-0000-0000-0000-000000000000", WorkspaceSlug: "whatever"}
		got, created, err := SelectWorkspace(target, records, nil, "shop")
		if err != nil {
			t.Fatal(err)
		}
		if created {
			t.Error("adopting an existing workspace was reported as created")
		}
		if got.WorkspaceID != records[0].ID {
			t.Errorf("WorkspaceID = %q, want the adopted %q", got.WorkspaceID, records[0].ID)
		}
	})

	t.Run("--workspace names a new workspace", func(t *testing.T) {
		target := Target{RemoteDir: "/opt/otter", Host: "h", WorkspaceID: "dddd4444-0000-0000-0000-000000000000", WorkspaceSlug: "ignored"}
		got, created, err := SelectWorkspace(target, records, nil, "analytics")
		if err != nil {
			t.Fatal(err)
		}
		if !created || got.WorkspaceName() != "analytics-dddd4444" {
			t.Errorf("created=%v name=%q, want a new analytics-dddd4444", created, got.WorkspaceName())
		}
	})
}

// The record scan mixes JSON records, a marker and raw port numbers; a host is
// not pristine, so anything unrecognised is skipped rather than fatal.
func TestParseWorkspaceList(t *testing.T) {
	out := `{"id":"aaaa1111-0000-0000-0000-000000000000","slug":"shop","name":"shop-aaaa1111","unit":"otterd-shop-aaaa1111","listen":"127.0.0.1:7337","created_at":"2026-01-01T00:00:00Z"}
this line is not json
` + workspacePortsMarker + `
7337
22
not-a-port
`
	records, ports, err := ParseWorkspaceList(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Name != "shop-aaaa1111" {
		t.Fatalf("records = %+v", records)
	}
	if len(ports) != 2 || ports[0] != 7337 || ports[1] != 22 {
		t.Errorf("ports = %v, want [7337 22]", ports)
	}
}

// Destroy must remove one workspace and leave every other one running. The
// paths it deletes are the ones that would otherwise take the whole host.
func TestDestroyScriptIsScopedToOneWorkspace(t *testing.T) {
	target := testTarget()
	script := DestroyScript(target, false)

	if strings.Contains(script, `rm -rf "$REMOTE_DIR"`) {
		t.Errorf("destroy removes the whole install root:\n%s", script)
	}
	if strings.Contains(script, `rm -rf "$ENV_DIR"`) {
		t.Errorf("destroy removes every workspace's environment file:\n%s", script)
	}
	for _, want := range []string{
		`rm -rf "$WORKSPACE_DIR"`,
		`rm -f "$SHARED_ENV" "$DAEMON_ENV"`,
		`rm -f "$UNIT"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("destroy script is missing %q:\n%s", want, script)
		}
	}

	// Keeping the data must keep exactly that: the workspace's state, and
	// nothing else in the tree.
	keeping := DestroyScript(target, true)
	if strings.Contains(keeping, `rm -rf "$WORKSPACE_DIR"`) {
		t.Errorf("--keep-data still removes the workspace:\n%s", keeping)
	}
	if !strings.Contains(keeping, `! -name `+ShellQuote(StateDirName)) {
		t.Errorf("--keep-data does not preserve the state directory:\n%s", keeping)
	}
}

// The record is written before anything else lands, so a retry finds the same
// port instead of starting a second workspace beside the first.
func TestWriteWorkspaceRecordScript(t *testing.T) {
	target := testTarget()
	rec := RecordFor(target, time.Now().UTC())
	script := WriteWorkspaceRecordScript(target, rec)

	if !strings.Contains(script, ShellQuote(target.WorkspaceRecordPath())) {
		t.Errorf("record script does not name the record path:\n%s", script)
	}
	if !strings.Contains(script, `"name":"`+target.WorkspaceName()+`"`) {
		t.Errorf("record script does not carry the workspace name:\n%s", script)
	}
	if !strings.Contains(script, `"listen":"`+target.Listen+`"`) {
		t.Errorf("record script does not carry the port:\n%s", script)
	}
}
