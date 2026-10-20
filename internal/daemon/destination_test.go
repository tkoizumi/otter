package daemon

import (
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/secrets"
)

// The manifest declares where a job's effects land, and the runtime has to
// REPORT it: the Cloud UI renders a destination column, and until the runtime
// sent the field it was empty for every job on every real runtime -- populated
// only by the cloud's own fixture, which is exactly the kind of gap a fixture
// hides. The wiring is asserted here rather than assumed.
func TestTheJobViewReportsDeclaredDestinationTables(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "castor", `
version: 1
name: castor
entrypoint: main.py
timeout: 30
destination_tables:
  - shopify_orders
  - castor.shopify_orders_archive
`, "print('x')\n")

	d := newDaemon(t, root, "", secrets.NewStaticProvider(map[string]string{}), nil)
	view, ok := d.GetJob("castor")
	if !ok {
		t.Fatal("the job was not found")
	}
	got := strings.Join(view.DestinationTables, ",")
	if want := "shopify_orders,castor.shopify_orders_archive"; got != want {
		t.Errorf("destination tables = %q, want %q", got, want)
	}
}

// A job that declares none reports none. That keeps "writes nowhere declared"
// distinguishable from a job whose destinations the runtime failed to send.
func TestTheJobViewReportsNoDestinationsForAJobThatDeclaresNone(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "plain", cronTicker, noopPython)

	d := newDaemon(t, root, "", secrets.NewStaticProvider(map[string]string{}), nil)
	view, ok := d.GetJob("ticker")
	if !ok {
		t.Fatal("the job was not found")
	}
	if len(view.DestinationTables) != 0 {
		t.Errorf("destination tables = %v, want none", view.DestinationTables)
	}
}
