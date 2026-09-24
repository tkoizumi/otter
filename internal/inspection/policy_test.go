package inspection

import "testing"

// TestDefaultPolicyIsFull pins the product decision: a run nobody configured
// records payloads, because the failure worth debugging is the unattended one
// and capture cannot be enabled after the fact.
func TestDefaultPolicyIsFull(t *testing.T) {
	if DefaultPolicy != PolicyFull {
		t.Fatalf("DefaultPolicy = %q, want full", DefaultPolicy)
	}
	got, err := ParsePolicy("")
	if err != nil || got != PolicyFull {
		t.Errorf(`ParsePolicy("") = %q, %v; want full`, got, err)
	}
}

func TestParsePolicyOverride(t *testing.T) {
	tests := []struct {
		in      string
		want    Policy
		wantErr bool
	}{
		{"", "", false},
		{"   ", "", false},
		{"off", PolicyOff, false},
		{"metadata", PolicyMetadata, false},
		{"full", PolicyFull, false},
		{"everything", "", true},
		{"FULL", "", true},
	}
	for _, tc := range tests {
		got, err := ParsePolicyOverride(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParsePolicyOverride(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ParsePolicyOverride(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

// An override and a default answer different questions: an empty override means
// "no opinion" so the integration and deployment defaults can be consulted,
// while an empty default means full.
func TestParsePolicyOverrideIsNotTheDefault(t *testing.T) {
	override, err := ParsePolicyOverride("")
	if err != nil {
		t.Fatalf("ParsePolicyOverride(\"\") error = %v", err)
	}
	if override != "" {
		t.Errorf("an empty override resolved to %q, want no policy", override)
	}
	if override == DefaultPolicy {
		t.Error("an empty override must not resolve to the default policy")
	}
}

// Coverage must describe the adapters a run actually installed, in a stable
// order, and must never echo a name this daemon does not know.
func TestCoverageNamesOnlyKnownAdapters(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{"none", nil, ""},
		{"urllib only", []string{AdapterURLLib}, "urllib"},
		{"canonical order", []string{AdapterHTTPX, AdapterURLLib}, "urllib, httpx"},
		{"all adapters", []string{AdapterHTTPX, AdapterRequests, AdapterURLLib}, "urllib, requests, httpx"},
		{"duplicates collapse", []string{AdapterURLLib, AdapterURLLib}, "urllib"},
		{"unknown names are dropped", []string{"carrier-pigeon", AdapterRequests}, "requests"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CoverageFor(tc.in); got != tc.want {
				t.Errorf("CoverageFor(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidAdapter(t *testing.T) {
	for _, adapter := range AllAdapters() {
		if !ValidAdapter(adapter) {
			t.Errorf("ValidAdapter(%q) = false, want true", adapter)
		}
	}
	for _, adapter := range []string{"", "urllib ", "URLLIB", "carrier-pigeon"} {
		if ValidAdapter(adapter) {
			t.Errorf("ValidAdapter(%q) = true, want false", adapter)
		}
	}
}
