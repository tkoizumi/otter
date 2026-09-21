package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/identity"
)

// resolveRef maps a user reference to a registered instance.
//
// The grammar is explicit rather than a guess:
//
//	id:<id>   the durable identity, always unambiguous
//	<path>    "." and "..", an absolute path, anything containing a path
//	          separator, or a literal otter.yaml: resolved against the
//	          process working directory and matched to the instance that owns
//	          that canonical directory
//	<label>   exactly one registered instance carrying that manifest label
//
// A bare label is never preferred over an identity and never resolves through
// ambiguity: two instances sharing a label is a conflict that names both
// candidates. An opaque identity typed without the prefix is accepted as a
// fallback so an id copied out of JSON still works.
func (d *Daemon) resolveRef(ref string) (*registered, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("integration reference is empty: %w", api.ErrInvalid)
	}

	if id, ok := strings.CutPrefix(ref, "id:"); ok {
		entry, found := d.reg.get(id)
		if !found {
			return nil, fmt.Errorf("integration %q: %w", ref, api.ErrNotFound)
		}
		return entry, nil
	}

	if looksLikePath(ref) {
		return d.resolvePathRef(ref)
	}

	matches := d.reg.byLabel(ref)
	switch len(matches) {
	case 0:
		if entry, found := d.reg.get(ref); found {
			return entry, nil
		}
		return nil, fmt.Errorf("integration %q: %w", ref, api.ErrNotFound)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("integration %q is ambiguous; use id:<id> or a path: %s: %w",
			ref, describeCandidates(matches), api.ErrConflict)
	}
}

// resolvePathRef resolves a filesystem reference through the registry's path
// ownership, never by matching a manifest name.
func (d *Daemon) resolvePathRef(ref string) (*registered, error) {
	target := ref
	if !filepath.IsAbs(target) {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ref, api.ErrNotFound)
		}
		target = filepath.Join(wd, ref)
	}
	canonical, err := identity.Canonical(target)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ref, api.ErrNotFound)
	}
	rec, found, err := d.ident.Store().PathRecord(context.Background(), canonical)
	if err != nil {
		return nil, err
	}
	if !found || rec.OwnerID.IsZero() {
		return nil, fmt.Errorf("%s: no registered integration at that path: %w", ref, api.ErrNotFound)
	}
	entry, ok := d.reg.get(rec.OwnerID.String())
	if !ok {
		return nil, fmt.Errorf("%s: %w", ref, api.ErrNotFound)
	}
	return entry, nil
}

// looksLikePath reports whether a reference can only be a filesystem path.
func looksLikePath(ref string) bool {
	switch {
	case ref == "." || ref == "..":
		return true
	case filepath.IsAbs(ref):
		return true
	case strings.ContainsAny(ref, `/\`):
		return true
	case filepath.Base(ref) == config.ManifestFileName:
		return true
	}
	return false
}

func describeCandidates(matches []*registered) string {
	parts := make([]string, 0, len(matches))
	for _, m := range matches {
		parts = append(parts, fmt.Sprintf("%s at %s", m.Integration.ID, m.Integration.Dir))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// observationError explains why an observation the registry does not own is
// not runnable, so the API can list it with the real cause.
func observationError(obs identity.Observation) string {
	switch {
	case obs.ScanError != "":
		return obs.ScanError
	case obs.Marker == identity.MarkerUnsafe || obs.Marker == identity.MarkerMalformed:
		if obs.MarkerError != "" {
			return obs.MarkerError
		}
		return "marker cannot be trusted"
	case !obs.ManifestPresent:
		return "no " + config.ManifestFileName
	case obs.ManifestError != "":
		return obs.ManifestError
	default:
		return "integration is not registered"
	}
}

// resolveLifecycleRef resolves a reference for an administrative operation,
// where a retired or deleted identity is still a legitimate target.
//
// The active registry is tried first. An explicit id, or a path whose recorded
// owner is no longer active, is then resolved against the identity store, so
// an integration whose directory has already been removed can still be purged
// by id. Deleting the source first and the data second must work.
func (d *Daemon) resolveLifecycleRef(ctx context.Context, ref string) (identity.Instance, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return identity.Instance{}, fmt.Errorf("integration reference is empty: %w", api.ErrInvalid)
	}
	if entry, err := d.resolveRef(ref); err == nil && !entry.Instance.ID.IsZero() {
		return entry.Instance, nil
	}

	if id, ok := strings.CutPrefix(ref, "id:"); ok {
		return d.instanceAnyStatus(ctx, id)
	}

	if looksLikePath(ref) {
		target := ref
		if !filepath.IsAbs(target) {
			wd, err := os.Getwd()
			if err != nil {
				return identity.Instance{}, err
			}
			target = filepath.Join(wd, ref)
		}
		canonical, err := identity.Canonical(target)
		if err != nil {
			return identity.Instance{}, fmt.Errorf("%s: %w", ref, api.ErrNotFound)
		}
		rec, found, err := d.ident.Store().PathRecord(ctx, canonical)
		if err != nil {
			return identity.Instance{}, err
		}
		if found && !rec.OwnerID.IsZero() {
			return d.instanceAnyStatus(ctx, rec.OwnerID.String())
		}
		return identity.Instance{}, fmt.Errorf("%s: no integration was ever registered there: %w", ref, api.ErrNotFound)
	}

	// A retired identity has left the active registry, so a label can only be
	// matched against the registry itself. One match is unambiguous; two
	// retired integrations sharing a label need the id.
	switch matches := d.instancesByLabel(ctx, ref); len(matches) {
	case 1:
		return matches[0], nil
	case 0:
	default:
		return identity.Instance{}, fmt.Errorf(
			"integration %q matches %d retired or deleted identities; use id:<id>: %w",
			ref, len(matches), api.ErrConflict)
	}

	// An opaque identity typed without the prefix, which for a legacy
	// workspace is often the label too.
	return d.instanceAnyStatus(ctx, ref)
}

// instancesByLabel returns every registered instance carrying a label,
// whatever its status. The active registry normally answers this; this is the
// fallback for an identity that has left it.
func (d *Daemon) instancesByLabel(ctx context.Context, label string) []identity.Instance {
	instances, err := d.ident.Store().Instances(ctx)
	if err != nil {
		return nil
	}
	var out []identity.Instance
	for _, inst := range instances {
		if inst.Name == label {
			out = append(out, inst)
		}
	}
	return out
}

func (d *Daemon) instanceAnyStatus(ctx context.Context, id string) (identity.Instance, error) {
	inst, err := d.ident.Store().Instance(ctx, identity.MustParseOrZero(id))
	if err != nil {
		return identity.Instance{}, fmt.Errorf("integration %q: %w", id, api.ErrNotFound)
	}
	return inst, nil
}
