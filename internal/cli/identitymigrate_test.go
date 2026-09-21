package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/daemon"
	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/datalock"
	"github.com/tkoizumi/otter/internal/identity"
	"github.com/tkoizumi/otter/internal/logging"
	"github.com/tkoizumi/otter/internal/release"
)

// seedLegacyWorkspace builds a workspace the way the previous model left it:
// an integration on disk and durable rows keyed by manifest name.
func seedLegacyWorkspace(t *testing.T) (root, dir, dataDir string) {
	t.Helper()

	root = t.TempDir()
	dataDir = filepath.Join(root, stateDirName, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(root, "counter")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntegrationFixture(t, dir, "counter", "print('ok')\n")

	ctx := context.Background()
	db, err := database.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate legacy database: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO integration_state (integration_id, key, value, updated_at) VALUES (?, ?, ?, ?)`,
		"counter", "count", "60", database.FormatTime(time.Now().UTC())); err != nil {
		t.Fatalf("seed legacy state: %v", err)
	}
	return root, dir, dataDir
}

// The end-to-end migration: a name-keyed workspace becomes an identity-keyed
// one, keeping the legacy key as the identity so state, history and tokens do
// not move, and the daemon then serves exactly the state that was there before.
func TestIdentityMigrateMovesLegacyStateAndTheDaemonServesIt(t *testing.T) {
	root, dir, dataDir := seedLegacyWorkspace(t)

	stdout, stderr, code := otterIn(t, root, "identity", "migrate", "--apply")
	if code != 0 {
		t.Fatalf("migrate exited %d:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "keep") || !strings.Contains(stdout, "counter") {
		t.Fatalf("migration did not report the assignment:\n%s", stdout)
	}

	// The legacy key became the identity, so nothing had to be renamed.
	id, err := identity.ReadMarker(dir)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if id.String() != "counter" {
		t.Fatalf("marker = %q, want the legacy key counter", id)
	}

	ctx := context.Background()
	cfg := config.DefaultDaemonConfig("test")
	cfg.IntegrationsDir = root
	cfg.DataDir = dataDir
	cfg.Workers = 1
	cfg.LogLevel = "error"
	d, err := daemon.New(ctx, daemon.Options{
		Config:  cfg,
		Logger:  logging.New(io.Discard, logging.FormatJSON, logging.LevelError),
		Version: "test",
	})
	if err != nil {
		t.Fatalf("daemon.New on a migrated workspace: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = d.Shutdown(shutdownCtx)
	})

	value, err := d.GetState(ctx, "counter", "count")
	if err != nil {
		t.Fatalf("state after migration: %v", err)
	}
	if string(value) != "60" {
		t.Fatalf("state = %s, want the legacy value 60", value)
	}
}

// A second migrate is a no-op: bootstrap is recorded, and re-running never
// reallocates an identity.
func TestIdentityMigrateIsIdempotent(t *testing.T) {
	root, dir, _ := seedLegacyWorkspace(t)

	if _, stderr, code := otterIn(t, root, "identity", "migrate", "--apply"); code != 0 {
		t.Fatalf("first migrate exited %d: %s", code, stderr)
	}
	first, err := identity.ReadMarker(dir)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}

	stdout, stderr, code := otterIn(t, root, "identity", "migrate", "--apply")
	if code != 0 {
		t.Fatalf("second migrate exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "already bootstrapped") {
		t.Fatalf("second migrate did not report a no-op:\n%s", stdout)
	}
	second, err := identity.ReadMarker(dir)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if second != first {
		t.Fatalf("identity changed on a second migrate: %q -> %q", first, second)
	}
}

// A dry run reports the plan and writes nothing.
func TestIdentityMigrateDryRunWritesNothing(t *testing.T) {
	root, dir, _ := seedLegacyWorkspace(t)

	stdout, stderr, code := otterIn(t, root, "identity", "migrate")
	if code != 0 {
		t.Fatalf("dry run exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "dry run") || !strings.Contains(stdout, "counter") {
		t.Fatalf("dry run did not report the plan:\n%s", stdout)
	}
	if identity.MarkerExists(dir) {
		t.Fatalf("a dry run wrote a marker")
	}
}

// A legacy name claimed by two directories needs an explicit owner: guessing
// would attach one directory's state to the other.
func TestIdentityMigrateRefusesACollisionWithoutAnOwner(t *testing.T) {
	root, _, _ := seedLegacyWorkspace(t)
	second := filepath.Join(root, "counter-copy")
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntegrationFixture(t, second, "counter", "print('copy')\n")

	_, stderr, code := otterIn(t, root, "identity", "migrate", "--apply")
	if code == 0 {
		t.Fatalf("migration accepted a collision without an owner")
	}
	if !strings.Contains(stderr, "--assign") || !strings.Contains(stderr, "counter") {
		t.Fatalf("refusal does not explain how to resolve it:\n%s", stderr)
	}

	// Naming the owner succeeds, and the other directory is left to register
	// fresh rather than share the legacy state.
	if _, stderr, code := otterIn(t, root, "identity", "migrate", "--apply", "--assign", "counter="+second); code != 0 {
		t.Fatalf("migrate with an owner exited %d:\n%s", code, stderr)
	}
	chosen, err := identity.ReadMarker(second)
	if err != nil || chosen.String() != "counter" {
		t.Fatalf("chosen marker = %q (%v), want counter", chosen, err)
	}
}

// Bootstrap writes markers, so it must own the data directory: a second writer
// racing a live runtime is exactly what the registry exists to prevent.
func TestIdentityMigrateRefusesWhileAnotherOwnerHoldsTheData(t *testing.T) {
	root, _, dataDir := seedLegacyWorkspace(t)

	lock, err := datalock.Acquire(dataDir)
	if err != nil {
		t.Fatalf("acquire data lock: %v", err)
	}
	defer func() { _ = lock.Close() }()

	_, stderr, code := otterIn(t, root, "identity", "migrate", "--apply")
	if code == 0 {
		t.Fatalf("migration ran while another owner held the data directory")
	}
	if !strings.Contains(stderr, "stop the runtime") {
		t.Fatalf("refusal does not explain itself:\n%s", stderr)
	}
}

// Choosing an owner does not make the identity's existing release trustworthy:
// a release staged from the directory the operator decided against would run
// the wrong code against the chosen tree's state. It is disabled, and the
// staged snapshot is kept rather than destroyed.
func TestIdentityMigrateQuarantinesAMismatchedRelease(t *testing.T) {
	root, chosen, dataDir := seedLegacyWorkspace(t)
	losing := filepath.Join(root, "counter-copy")
	if err := os.MkdirAll(losing, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntegrationFixture(t, losing, "counter", "print('copy')\n")

	// Stage and activate a release for the legacy name from the losing
	// directory, the way the incident did.
	manager := release.Manager{DataDir: dataDir}
	layout, err := release.Plan(root, losing, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	meta, err := manager.StageWithLayout("counter", losing, layout, "")
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if err := manager.Activate("counter", meta.Digest); err != nil {
		t.Fatalf("activate: %v", err)
	}

	// A queued attempt already pinned to that release.
	ctx := context.Background()
	seed, err := database.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := database.Migrate(ctx, seed); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := seed.ExecContext(ctx,
		`INSERT INTO runs (id, integration_id, trigger_type, status, attempt, created_at, release_digest)
		 VALUES ('pinned', 'counter', 'manual', 'queued', 1, ?, ?)`,
		database.FormatTime(time.Now().UTC()), meta.Digest); err != nil {
		t.Fatalf("seed pinned run: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	if _, stderr, code := otterIn(t, root, "identity", "migrate", "--apply", "--assign", "counter="+chosen); code != 0 {
		t.Fatalf("migrate exited %d:\n%s", code, stderr)
	}

	// The pointer is gone; the snapshot survives.
	if _, ok, err := manager.Active("counter"); err != nil || ok {
		t.Fatalf("a release staged from the losing directory stayed active: ok=%v err=%v", ok, err)
	}
	if _, err := manager.Metadata("counter", meta.Digest); err != nil {
		t.Fatalf("quarantine destroyed the staged release: %v", err)
	}

	// The attempt pinned to it is cancelled rather than executed against the
	// owner the operator chose.
	check, err := database.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = check.Close() }()
	var status, message string
	if err := check.QueryRowContext(ctx,
		`SELECT status, COALESCE(error, '') FROM runs WHERE id = 'pinned'`).Scan(&status, &message); err != nil {
		t.Fatalf("read pinned run: %v", err)
	}
	if status != "cancelled" {
		t.Fatalf("pinned run status = %q, want cancelled", status)
	}
	if !strings.Contains(message, "quarantined") {
		t.Fatalf("pinned run error does not explain itself: %q", message)
	}
}
