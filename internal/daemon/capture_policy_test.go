package daemon

import (
	"testing"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/inspection"
)

// TestResolveCapturePolicyPrecedence pins the rule that makes payload capture
// safe to have on by default: an integration can always refuse, and an explicit
// per-run request wins over everything.
func TestResolveCapturePolicyPrecedence(t *testing.T) {
	tests := []struct {
		name     string
		deploy   inspection.Policy
		manifest *config.Manifest
		override inspection.Policy
		want     inspection.Policy
	}{
		{
			name:   "deployment default applies when nothing else chooses",
			deploy: inspection.PolicyMetadata,
			want:   inspection.PolicyMetadata,
		},
		{
			name: "built-in default is full when the deployment leaves it unset",
			want: inspection.PolicyFull,
		},
		{
			name:     "a manifest capture off refuses capture",
			deploy:   inspection.PolicyFull,
			manifest: &config.Manifest{Capture: "off"},
			want:     inspection.PolicyOff,
		},
		{
			name:     "a manifest capture full survives a metadata deployment",
			deploy:   inspection.PolicyMetadata,
			manifest: &config.Manifest{Capture: "full"},
			want:     inspection.PolicyFull,
		},
		{
			name:     "a manifest without an opinion falls through",
			deploy:   inspection.PolicyMetadata,
			manifest: &config.Manifest{},
			want:     inspection.PolicyMetadata,
		},
		{
			name:     "a per-run override beats the manifest",
			deploy:   inspection.PolicyMetadata,
			manifest: &config.Manifest{Capture: "off"},
			override: inspection.PolicyFull,
			want:     inspection.PolicyFull,
		},
		{
			name:     "a per-run off beats a full manifest",
			deploy:   inspection.PolicyFull,
			manifest: &config.Manifest{Capture: "full"},
			override: inspection.PolicyOff,
			want:     inspection.PolicyOff,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := &Daemon{cfg: config.DaemonConfig{CaptureDefault: tc.deploy}}
			got, err := d.resolveCapturePolicy(tc.override, tc.manifest)
			if err != nil {
				t.Fatalf("resolveCapturePolicy() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("policy = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveCapturePolicyRejectsUnknownOverride(t *testing.T) {
	d := &Daemon{cfg: config.DefaultDaemonConfig("test")}
	if _, err := d.resolveCapturePolicy(inspection.Policy("everything"), nil); err == nil {
		t.Error("an unknown per-run policy was accepted")
	}
}

// TestCapturePolicyForShowsWhatARunWouldUse is what `otter inspect` relies on,
// and it is computed the same way a submission is, so the displayed policy
// cannot drift from the recorded one.
func TestCapturePolicyForShowsWhatARunWouldUse(t *testing.T) {
	d := &Daemon{cfg: config.DefaultDaemonConfig("test")}
	if got := d.CapturePolicyFor(&config.Manifest{Capture: "off"}); got != inspection.PolicyOff {
		t.Errorf("CapturePolicyFor(off) = %q, want off", got)
	}
	if got := d.CapturePolicyFor(&config.Manifest{Capture: "metadata"}); got != inspection.PolicyMetadata {
		t.Errorf("CapturePolicyFor(metadata) = %q, want metadata", got)
	}
	if got := d.CapturePolicyFor(nil); got != inspection.PolicyFull {
		t.Errorf("CapturePolicyFor(nil) = %q, want full", got)
	}
	if got := d.CapturePolicyFor(&config.Manifest{}); got != inspection.PolicyFull {
		t.Errorf("CapturePolicyFor(no opinion) = %q, want full", got)
	}
}
