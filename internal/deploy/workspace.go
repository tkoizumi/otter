package deploy

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// This file is how one host holds several workspaces without them colliding.
//
// A workspace is identified on the host by a record under
// <remote>/workspaces/<name>/workspace.json. The record is what makes a deploy
// from a second machine, a renamed directory or a fresh clone land on the same
// directory, unit and port instead of creating a second copy of everything --
// and it is how a new workspace finds a port nobody else holds.

// WorkspaceRecord is what the host remembers about one workspace.
type WorkspaceRecord struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	Unit      string    `json:"unit"`
	Listen    string    `json:"listen"`
	CreatedAt time.Time `json:"created_at"`
}

// workspacePortsMarker separates the records from the list of ports in use.
const workspacePortsMarker = "__otter_listening_ports__"

// WorkspaceListScript prints every workspace record on the host, one JSON
// object per line, followed by the marker and the loopback ports currently
// listening. Both are needed to choose a port: a port can be claimed by a
// workspace whose daemon is stopped, or held by something else entirely.
func WorkspaceListScript(t Target) string {
	return `ROOT=` + ShellQuote(t.WorkspacesRoot()) + `
for f in "$ROOT"/*/` + WorkspaceRecordName + `; do
  [ -f "$f" ] || continue
  cat "$f"; echo
done
echo ` + ShellQuote(workspacePortsMarker) + `
ss -ltnH 2>/dev/null | awk '{print $4}' | sed 's/.*://' | sort -u || true
`
}

// WriteWorkspaceRecordScript writes one workspace record.
//
// It is written as soon as the workspace is resolved, before anything else
// lands, so a deploy that fails half way does not hand the next attempt a
// different port.
func WriteWorkspaceRecordScript(t Target, rec WorkspaceRecord) string {
	body, err := json.Marshal(rec)
	if err != nil {
		// The record is plain strings and a timestamp.
		panic(fmt.Sprintf("marshal workspace record: %v", err))
	}
	return `set -e
RECORD=` + ShellQuote(t.WorkspaceRecordPath()) + `
RUN_AS=` + ShellQuote(t.RunAsUser) + `
WORKSPACE_DIR=` + ShellQuote(t.WorkspaceDir()) + `
install -d -m 0755 -o "$RUN_AS" -g "$RUN_AS" "$WORKSPACE_DIR"
cat > "$RECORD" <<'OTTER_WORKSPACE_EOF'
` + string(body) + `
OTTER_WORKSPACE_EOF
chmod 0644 "$RECORD"
chown "$RUN_AS:$RUN_AS" "$RECORD" 2>/dev/null || true
`
}

// ParseWorkspaceList splits the output of WorkspaceListScript.
//
// A line that is not a record and not the marker is skipped rather than
// failing: the host is not expected to be pristine, and one unreadable record
// must not stop a deploy that would otherwise work.
func ParseWorkspaceList(out string) ([]WorkspaceRecord, []int, error) {
	var records []WorkspaceRecord
	var ports []int

	inPorts := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == workspacePortsMarker {
			inPorts = true
			continue
		}
		if inPorts {
			if port, err := strconv.Atoi(line); err == nil && port > 0 && port < 65536 {
				ports = append(ports, port)
			}
			continue
		}
		var rec WorkspaceRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		records = append(records, rec)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Name < records[j].Name })
	return records, ports, nil
}

// NextListenPort is the lowest loopback port at or above DefaultListenPort that
// no workspace claims and nothing is listening on.
func NextListenPort(records []WorkspaceRecord, listening []int) int {
	used := map[int]bool{}
	for _, rec := range records {
		if port := portOf(rec.Listen); port > 0 {
			used[port] = true
		}
	}
	for _, port := range listening {
		used[port] = true
	}
	// 1000 workspaces is far past any real host; the bound only stops an
	// unbounded scan if something is wrong with the port list.
	for port := DefaultListenPort; port < DefaultListenPort+1000; port++ {
		if !used[port] {
			return port
		}
	}
	return DefaultListenPort + 1000
}

// SelectWorkspace decides which workspace on the host this deploy owns, and
// returns the target with that workspace's canonical name, unit and port.
//
// The boolean reports whether a new workspace was created, which is what lets
// the deploy say so rather than silently starting a second daemon.
func SelectWorkspace(t Target, records []WorkspaceRecord, listening []int, requested string) (Target, bool, error) {
	// An explicit --workspace adopts an existing workspace by name, slug or id,
	// or creates one under that name. Adoption is how a machine with no local
	// state deploys into a workspace that already exists.
	if strings.TrimSpace(requested) != "" {
		if rec, ok := matchWorkspace(records, requested); ok {
			return adoptWorkspace(t, rec).fillWorkspaceDefaults(), false, nil
		}
		// A project that already owns a workspace on this host needs a new
		// identity for a second one: a bare deploy matches by id, and two
		// records sharing one id would make that match ambiguous.
		for _, rec := range records {
			if rec.ID != "" && rec.ID == t.WorkspaceID {
				t.WorkspaceID = NewWorkspaceID()
				break
			}
		}
		t.WorkspaceSlug = SanitizeSlug(requested)
		t.WorkspaceNameOverride = ""
		return withListenPort(t, records, listening), true, nil
	}

	// The same project, however it was reached: same id, same workspace.
	for _, rec := range records {
		if rec.ID != "" && rec.ID == t.WorkspaceID {
			return adoptWorkspace(t, rec).fillWorkspaceDefaults(), false, nil
		}
	}

	return withListenPort(t, records, listening), true, nil
}

// withListenPort gives the target the next free loopback port unless one was
// named, then completes the paths that follow from the workspace name.
func withListenPort(t Target, records []WorkspaceRecord, listening []int) Target {
	if strings.TrimSpace(t.Listen) == "" {
		t.Listen = net.JoinHostPort(DefaultListenHost, strconv.Itoa(NextListenPort(records, listening)))
	}
	return t.fillWorkspaceDefaults()
}

// adoptWorkspace returns the target with the recorded workspace's identity.
//
// The workspace's slug is taken from the record, not from the local directory,
// so renaming the project does not orphan the tree on the host. Data and unit
// names that were only ever local defaults are recomputed; anything the
// operator set explicitly is kept.
func adoptWorkspace(t Target, rec WorkspaceRecord) Target {
	dataWasDefault := t.DataDir == "" || t.DataDir == filepath.Join(t.WorkspaceDir(), StateDirName, "data")
	serviceWasDefault := t.ServiceName == "" || t.ServiceName == DefaultServicePrefix+"-"+t.WorkspaceName()

	t.WorkspaceID = rec.ID
	t.WorkspaceSlug = rec.Slug
	// The recorded name wins outright: it names a directory, a unit and an
	// environment file that already exist, so reproducing it is not optional.
	t.WorkspaceNameOverride = strings.TrimSpace(rec.Name)
	t.Listen = rec.Listen
	if dataWasDefault {
		t.DataDir = filepath.Join(t.WorkspaceDir(), StateDirName, "data")
	}
	if serviceWasDefault {
		if strings.TrimSpace(rec.Unit) != "" {
			t.ServiceName = rec.Unit
		} else {
			t.ServiceName = DefaultServicePrefix + "-" + t.WorkspaceName()
		}
	}
	return t
}

// matchWorkspace finds a workspace by name, slug or id, accepting an
// abbreviated id.
func matchWorkspace(records []WorkspaceRecord, requested string) (WorkspaceRecord, bool) {
	want := strings.ToLower(strings.TrimSpace(requested))
	for _, rec := range records {
		switch {
		case strings.ToLower(rec.Name) == want,
			strings.ToLower(rec.Slug) == want,
			strings.ToLower(rec.ID) == want:
			return rec, true
		}
		if short := shortID(rec.ID); short != "" && want == short {
			return rec, true
		}
	}
	return WorkspaceRecord{}, false
}

// RecordFor renders the record a target should have on the host.
func RecordFor(t Target, created time.Time) WorkspaceRecord {
	return WorkspaceRecord{
		ID:        t.WorkspaceID,
		Slug:      t.WorkspaceSlug,
		Name:      t.WorkspaceName(),
		Unit:      t.ServiceName,
		Listen:    t.Listen,
		CreatedAt: created,
	}
}

// workspaceNames lists what a host holds, for an error message.
func workspaceNames(records []WorkspaceRecord) string {
	if len(records) == 0 {
		return "none"
	}
	names := make([]string, 0, len(records))
	for _, rec := range records {
		names = append(names, rec.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// portOf extracts the port from a "host:port" listen address.
func portOf(listen string) int {
	_, port, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return 0
	}
	return n
}
