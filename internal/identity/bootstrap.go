package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// This file implements the one-time semantic bootstrap that moves a workspace
// from name-keyed durable data to the identity registry.
//
// The SQL migration and this step are deliberately separate. Advancing the
// schema leaves the rows' keys untouched; only bootstrap decides which
// directory owns a legacy name, and it refuses to guess. An unambiguous
// workspace keeps `id = old_name`, so no state, history, token or release has
// to move. A collision needs an operator's choice, because there is only one
// old state namespace and inventing a split would attach the wrong data.

// Assignment records one legacy key handed to a source directory.
type Assignment struct {
	ID   ID
	Name string
	Path string
}

// BootstrapConflict is a legacy key claimed by more than one directory.
type BootstrapConflict struct {
	Name  string
	Paths []string
}

// BootstrapResult reports what a bootstrap did or would do.
type BootstrapResult struct {
	Assigned []Assignment
	// Reserved lists legacy keys kept as retired instances because no source
	// directory claims them. Reserving them is what stops a future integration
	// from reusing a name that still has state and history.
	Reserved []ID
	// Fresh lists directories that had no legacy key and will register new.
	Fresh     []string
	Conflicts []BootstrapConflict
}

// LegacyKeys inventories every integration key durable rows already use. It is
// inventory, not ownership: the values are the previous model's manifest
// names, and every one of them has to be accounted for -- assigned to a source
// or reserved -- before the registry can be trusted.
func LegacyKeys(ctx context.Context, db *sql.DB) ([]string, error) {
	seen := map[string]bool{}
	for _, query := range []string{
		`SELECT DISTINCT integration_id FROM integration_state`,
		`SELECT DISTINCT integration_id FROM runs`,
		`SELECT DISTINCT integration_id FROM run_queue`,
		`SELECT DISTINCT integration_id FROM webhook_tokens`,
	} {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("identity: inventory legacy keys: %w", err)
		}
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return nil, err
			}
			if key != "" {
				seen[key] = true
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	out := make([]string, 0, len(seen))
	for key := range seen {
		out = append(out, key)
	}
	sort.Strings(out)
	return out, nil
}

// BootstrapConflictError reports legacy keys that cannot be assigned without
// an operator decision.
type BootstrapConflictError struct {
	Conflicts []BootstrapConflict
}

func (e *BootstrapConflictError) Error() string {
	parts := make([]string, 0, len(e.Conflicts))
	for _, c := range e.Conflicts {
		parts = append(parts, fmt.Sprintf("%q is declared by %s", c.Name, strings.Join(c.Paths, ", ")))
	}
	return "identity: legacy name collision needs an explicit owner: " + strings.Join(parts, "; ")
}

// BootstrapLegacy assigns legacy keys to source directories.
//
// assignments maps a legacy key to the canonical directory that should keep
// it, which is the operator's answer to a collision. A key with exactly one
// candidate is assigned automatically; a key with several is a conflict unless
// an assignment resolves it; a key with none is reserved as retired.
//
// It writes markers and creates instances but does not reconcile the rest of
// the tree: the caller's ordinary scan does that afterwards, so an extra copy
// of a legacy integration is registered fresh rather than stealing the
// original's data.
func (s *Service) BootstrapLegacy(ctx context.Context, scan Scan, legacyKeys []string, assignments map[string]string, dryRun bool) (BootstrapResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var result BootstrapResult
	if len(legacyKeys) == 0 {
		return result, nil
	}

	byLabel := map[string][]Observation{}
	for _, obs := range scan.Observations {
		if obs.Exists && obs.ManifestValid && obs.Name != "" {
			byLabel[obs.Name] = append(byLabel[obs.Name], obs)
		}
	}
	for label := range byLabel {
		sort.Slice(byLabel[label], func(i, j int) bool { return byLabel[label][i].Path < byLabel[label][j].Path })
	}

	keys := append([]string(nil), legacyKeys...)
	sort.Strings(keys)

	// Decide first, mutate second. A collision must leave the registry exactly
	// as it was: a bootstrap that assigned some keys and then refused the rest
	// would strand the remaining keys' state outside the registry, and a later
	// start would see a non-empty registry and consider itself done.
	type decision struct {
		key  string
		id   ID
		kind string
		obs  Observation
	}
	var decisions []decision

	for _, key := range keys {
		id, err := Parse(key)
		if err != nil {
			// A legacy key that is not a safe identifier cannot be preserved;
			// it is reported rather than written as a marker.
			result.Conflicts = append(result.Conflicts, BootstrapConflict{Name: key, Paths: []string{"(not a usable identifier)"}})
			continue
		}
		if _, err := s.store.Instance(ctx, id); err == nil {
			continue // already bootstrapped
		} else if !errors.Is(err, ErrNotFound) {
			return result, err
		}

		candidates := byLabel[key]
		switch {
		case len(candidates) == 0:
			decisions = append(decisions, decision{key: key, id: id, kind: "reserve"})

		case len(candidates) == 1:
			decisions = append(decisions, decision{key: key, id: id, kind: "assign", obs: candidates[0]})

		default:
			chosen := ""
			if want := strings.TrimSpace(assignments[key]); want != "" {
				canonical, err := Canonical(want)
				if err != nil {
					return result, err
				}
				for _, obs := range candidates {
					if obs.Path == canonical {
						chosen = obs.Path
						break
					}
				}
				if chosen == "" {
					return result, fmt.Errorf("identity: --assign %s=%s names a directory that does not declare that name", key, want)
				}
			}
			if chosen == "" {
				paths := make([]string, 0, len(candidates))
				for _, obs := range candidates {
					paths = append(paths, obs.Path)
				}
				result.Conflicts = append(result.Conflicts, BootstrapConflict{Name: key, Paths: paths})
				continue
			}
			for _, obs := range candidates {
				if obs.Path != chosen {
					result.Fresh = append(result.Fresh, obs.Path)
					continue
				}
				decisions = append(decisions, decision{key: key, id: id, kind: "assign", obs: obs})
			}
		}
	}

	if len(result.Conflicts) > 0 {
		return result, &BootstrapConflictError{Conflicts: result.Conflicts}
	}

	// Apply only now that every key has an answer.
	for _, d := range decisions {
		switch d.kind {
		case "reserve":
			if !dryRun {
				if err := s.reserveLegacy(ctx, d.id, d.key); err != nil {
					return result, err
				}
			}
			result.Reserved = append(result.Reserved, d.id)
		case "assign":
			if !dryRun {
				if err := s.assignLegacy(ctx, d.id, d.key, d.obs); err != nil {
					return result, err
				}
			}
			result.Assigned = append(result.Assigned, Assignment{ID: d.id, Name: d.key, Path: d.obs.Path})
		}
	}
	return result, nil
}

// assignLegacy gives a legacy key to one directory and marks it as the owner.
func (s *Service) assignLegacy(ctx context.Context, id ID, name string, obs Observation) error {
	if err := WriteMarker(obs.Path, id); err != nil {
		return fmt.Errorf("identity: write legacy marker in %s: %w", obs.Path, err)
	}
	return s.store.CreateInstance(ctx, Instance{
		ID:            id,
		Name:          name,
		CanonicalPath: obs.Path,
		Status:        StatusActive,
		Generation:    1,
		CreatedAt:     time.Now().UTC(),
	})
}

// reserveLegacy keeps a key that no directory claims, so its state and history
// stay addressable and the key can never be handed to a new integration.
func (s *Service) reserveLegacy(ctx context.Context, id ID, name string) error {
	now := time.Now().UTC()
	return s.store.CreateInstance(ctx, Instance{
		ID:               id,
		Name:             name,
		CanonicalPath:    "",
		Status:           StatusRetired,
		Generation:       1,
		CreatedAt:        now,
		RetiredAt:        &now,
		RetirementReason: "legacy identity with no source directory",
	})
}
