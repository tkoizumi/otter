package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tkoizumi/otter/internal/identity"
)

func writeNamedManifest(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := "version: 1\nname: " + name + "\nentrypoint: main.py\n"
	if err := os.WriteFile(filepath.Join(dir, ManifestFileName), []byte(body), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("# entrypoint\n"), 0o644); err != nil {
		t.Fatalf("write entrypoint: %v", err)
	}
}

func observationFor(t *testing.T, scan identity.Scan, dir string) identity.Observation {
	t.Helper()
	canonical, err := identity.Canonical(dir)
	if err != nil {
		t.Fatalf("canonical %s: %v", dir, err)
	}
	for _, obs := range scan.Observations {
		if obs.Path == canonical {
			return obs
		}
	}
	t.Fatalf("no observation for %s in %+v", canonical, scan.Observations)
	return identity.Observation{}
}

func TestObserveReportsManifestsAndMarkers(t *testing.T) {
	root := t.TempDir()
	writeNamedManifest(t, filepath.Join(root, "alpha"), "alpha")
	writeNamedManifest(t, filepath.Join(root, "group", "beta"), "beta")

	scan, err := Observe(root, nil)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !scan.Complete {
		t.Fatalf("scan reported incomplete: %v", scan.Errors)
	}
	if len(scan.Observations) != 2 {
		t.Fatalf("observations = %+v, want 2", scan.Observations)
	}

	alpha := observationFor(t, scan, filepath.Join(root, "alpha"))
	if !alpha.Exists || !alpha.ManifestPresent || !alpha.ManifestValid || alpha.Name != "alpha" {
		t.Fatalf("alpha observation = %+v", alpha)
	}
	if alpha.Marker != identity.MarkerNone {
		t.Fatalf("alpha marker = %v, want none", alpha.Marker)
	}

	// A valid marker is reported as such.
	if err := identity.WriteMarker(filepath.Join(root, "alpha"), identity.MustParse("id-alpha")); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	scan, _ = Observe(root, nil)
	alpha = observationFor(t, scan, filepath.Join(root, "alpha"))
	if alpha.Marker != identity.MarkerValid || alpha.MarkerID != identity.MustParse("id-alpha") {
		t.Fatalf("alpha marker = %v %q, want valid id-alpha", alpha.Marker, alpha.MarkerID)
	}
}

func TestObserveReportsMalformedMarkerWithoutFailingScan(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "alpha")
	writeNamedManifest(t, dir, "alpha")
	if err := os.WriteFile(identity.MarkerPath(dir), []byte("two\nlines\n"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	scan, err := Observe(root, nil)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !scan.Complete {
		t.Fatalf("a malformed marker made the scan incomplete: %v", scan.Errors)
	}
	obs := observationFor(t, scan, dir)
	if obs.Marker != identity.MarkerMalformed {
		t.Fatalf("marker state = %v, want malformed", obs.Marker)
	}
}

func TestObserveReportsKnownPathWithoutManifest(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "alpha")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	scan, err := Observe(root, []string{dir})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	obs := observationFor(t, scan, dir)
	if !obs.Exists || obs.ManifestPresent {
		t.Fatalf("observation = %+v, want an existing directory without a manifest", obs)
	}
}

func TestObserveReportsKnownGonePath(t *testing.T) {
	root := t.TempDir()
	gone := filepath.Join(root, "gone")
	scan, err := Observe(root, []string{gone})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	obs := observationFor(t, scan, gone)
	if obs.Exists {
		t.Fatalf("observation = %+v, want Exists=false", obs)
	}
}

func TestObserveIncompleteOnUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	writeNamedManifest(t, filepath.Join(root, "alpha"), "alpha")
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	scan, err := Observe(root, nil)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if scan.Complete {
		t.Fatalf("scan claimed to be complete despite an unreadable directory")
	}
	if len(scan.Errors) == 0 {
		t.Fatalf("scan reported no errors")
	}
}
