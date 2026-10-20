package inspection

import (
	"context"
	"testing"
)

func TestNewExporterDefaultsToNoop(t *testing.T) {
	for _, mode := range []string{"", "none", "NONE", " none "} {
		exporter, err := NewExporter(mode)
		if err != nil {
			t.Fatalf("NewExporter(%q): %v", mode, err)
		}
		if _, ok := exporter.(NoopExporter); !ok {
			t.Fatalf("NewExporter(%q) = %T, want NoopExporter", mode, exporter)
		}
	}
}

func TestNewExporterRefusesAModeThisBuildDoesNotImplement(t *testing.T) {
	for _, mode := range []string{"outbox", "sink", "everywhere"} {
		if _, err := NewExporter(mode); err == nil {
			t.Fatalf("NewExporter(%q) accepted a mode with no implementation", mode)
		}
	}
}

func TestNoopExporterNeverTouchesTheTransaction(t *testing.T) {
	// A nil DBTX would panic if the no-op tried to use it. That is the point:
	// "none" must be a seam that changes no behaviour, not a best-effort write
	// that happens to do nothing.
	if err := (NoopExporter{}).Export(context.Background(), nil, []ExportRecord{
		{Kind: ExportExchange, RunID: "run-a", RequestID: "req-1"},
	}); err != nil {
		t.Fatalf("no-op export: %v", err)
	}
}

func TestStoreDefaultsToTheNoopExporter(t *testing.T) {
	store := NewStore(nil, nil, Limits{})
	if _, ok := store.exporter.(NoopExporter); !ok {
		t.Fatalf("default exporter = %T, want NoopExporter", store.exporter)
	}
	// A nil exporter is a no-op, never a nil pointer the ingest path could call.
	store.WithExporter(nil)
	if _, ok := store.exporter.(NoopExporter); !ok {
		t.Fatalf("WithExporter(nil) = %T, want NoopExporter", store.exporter)
	}
}

func TestParseExportModeIsExplicit(t *testing.T) {
	got, err := ParseExportMode("")
	if err != nil || got != ExportModeNone {
		t.Fatalf("ParseExportMode(\"\") = %q, %v; want none, nil", got, err)
	}
	if _, err := ParseExportMode("outbox"); err == nil {
		t.Fatal("ParseExportMode accepted outbox before its implementation exists")
	}
}
