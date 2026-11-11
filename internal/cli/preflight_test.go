package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestCredentialPreflightMatchesDeclaredNamesToConnections(t *testing.T) {
	connections := []ConnectionCredential{
		{System: "shopify", Name: "acme.myshopify.com", Credentials: []string{"SHOPIFY_CLIENT_ID", "SHOPIFY_CLIENT_SECRET"}},
		{System: "hubspot", Name: "HubSpot", Credentials: []string{"HUBSPOT_ACCESS_TOKEN"}},
	}

	provided, unprovided := credentialPreflight(
		[]string{"SHOPIFY_CLIENT_ID", "SHOPIFY_TOKEN", "HUBSPOT_ACCESS_TOKEN", "SHOPIFY_CLIENT_ID"},
		connections,
	)
	if strings.Join(provided, ",") != "HUBSPOT_ACCESS_TOKEN,SHOPIFY_CLIENT_ID" {
		t.Errorf("provided = %v, want the two names a connection covers (deduplicated and sorted)", provided)
	}
	if strings.Join(unprovided, ",") != "SHOPIFY_TOKEN" {
		t.Errorf("unprovided = %v, want only SHOPIFY_TOKEN", unprovided)
	}
}

func TestCredentialPreflightIsSilentWithoutSecretsOrConnections(t *testing.T) {
	if provided, unprovided := credentialPreflight(nil, nil); len(provided) != 0 || len(unprovided) != 0 {
		t.Errorf("a job with no secrets must produce no warning, got %v / %v", provided, unprovided)
	}
	provided, unprovided := credentialPreflight([]string{"ONLY_THIS"}, nil)
	if len(provided) != 0 || len(unprovided) != 1 {
		t.Errorf("no connections means nothing is provided, got %v / %v", provided, unprovided)
	}
}

func TestReportCredentialPreflightNamesWhatWillFail(t *testing.T) {
	var buffer bytes.Buffer
	reportCredentialPreflight(&buffer, "orders", []string{"SHOPIFY_CLIENT_ID"}, []string{"SHOPIFY_TOKEN"})
	out := buffer.String()
	if !strings.Contains(out, "SHOPIFY_CLIENT_ID  provided by a connection") {
		t.Errorf("the provided name is not reported:\n%s", out)
	}
	if !strings.Contains(out, "SHOPIFY_TOKEN  NO connection provides this") {
		t.Errorf("the unprovided name is not called out:\n%s", out)
	}
	// A silent run when everything is covered: warnings that fire always are
	// warnings nobody reads.
	buffer.Reset()
	reportCredentialPreflight(&buffer, "orders", []string{"SHOPIFY_CLIENT_ID"}, nil)
	if strings.Contains(buffer.String(), "NO connection") {
		t.Errorf("a covered job must not warn about missing credentials:\n%s", buffer.String())
	}
}
