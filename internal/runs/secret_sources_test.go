package runs

import (
	"context"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/secrets"
)

func TestSecretSourcesAreReportedPerNameAndOverwrittenByTheLatestRun(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	if err := store.RecordSecretSources(ctx, "job-1", "run-1", map[string]secrets.Source{
		"SHOPIFY_CLIENT_ID":        secrets.SourcePlatform,
		"SALESFORCE_AUTHORIZATION": secrets.SourceMissing,
	}, now); err != nil {
		t.Fatalf("record: %v", err)
	}

	got, err := store.SecretSources(ctx, "job-1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got["SHOPIFY_CLIENT_ID"] != "platform" {
		t.Errorf("SHOPIFY_CLIENT_ID = %q, want platform", got["SHOPIFY_CLIENT_ID"])
	}
	if got["SALESFORCE_AUTHORIZATION"] != "missing" {
		t.Errorf("SALESFORCE_AUTHORIZATION = %q, want missing", got["SALESFORCE_AUTHORIZATION"])
	}

	// A later run that resolved the second name from the environment replaces the
	// stale "missing" — the table is the latest observation, not a history.
	if err := store.RecordSecretSources(ctx, "job-1", "run-2", map[string]secrets.Source{
		"SHOPIFY_CLIENT_ID":        secrets.SourceEnvironment,
		"SALESFORCE_AUTHORIZATION": secrets.SourcePlatform,
	}, now.Add(time.Minute)); err != nil {
		t.Fatalf("record second run: %v", err)
	}
	got, _ = store.SecretSources(ctx, "job-1")
	if got["SHOPIFY_CLIENT_ID"] != "environment" || got["SALESFORCE_AUTHORIZATION"] != "platform" {
		t.Errorf("latest run did not win: %#v", got)
	}

	byJob, err := store.SecretSourcesByJob(ctx)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	if byJob["job-1"]["SHOPIFY_CLIENT_ID"] != "environment" {
		t.Errorf("listing view = %#v", byJob)
	}

	// Deleting a job must not leave a claim about its credentials behind.
	if _, err := store.DeleteByJob(ctx, "job-1"); err != nil {
		t.Fatalf("delete job: %v", err)
	}
	after, _ := store.SecretSources(ctx, "job-1")
	if len(after) != 0 {
		t.Errorf("secret sources survived the job: %#v", after)
	}
}
