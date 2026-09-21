package identity

import (
	"fmt"
	"sort"
)

// MarkerState describes what a scan found at a directory's marker path.
type MarkerState string

// Marker observations.
const (
	MarkerNone      MarkerState = "none"
	MarkerValid     MarkerState = "valid"
	MarkerMalformed MarkerState = "malformed"
	MarkerUnsafe    MarkerState = "unsafe"
)

// Observation is what one scan learned about one directory. A scan produces an
// observation for every discovered integration directory and for every known
// registered path, so that a registered path missing from the manifest walk is
// still described explicitly rather than inferred from absence.
type Observation struct {
	// Path is the canonical directory.
	Path string
	// Exists reports whether the directory exists at all.
	Exists bool
	// ManifestPresent reports whether otter.yaml is there.
	ManifestPresent bool
	// ManifestValid reports whether it parsed and validated.
	ManifestValid bool
	// ManifestError explains an invalid or unreadable manifest.
	ManifestError string
	// Name is the manifest label, when one could be read.
	Name string
	// Marker is the state of the `.otter-id` file.
	Marker MarkerState
	// MarkerID is the identifier the marker claims, when it is valid.
	MarkerID ID
	// MarkerError explains a malformed or unsafe marker.
	MarkerError string
	// ScanError reports a directory that could not be read at all.
	ScanError string
}

// Scan is one walk of the integrations root.
type Scan struct {
	// Complete is false when any directory or manifest could not be read. An
	// incomplete scan is evidence of a failed observation, never of deletion.
	Complete     bool
	Observations []Observation
	// Errors names every path that could not be read.
	Errors []string
}

// ActionKind is one reconciliation decision.
type ActionKind string

// Reconciliation actions.
const (
	// ActionPreserve keeps an existing identity and refreshes its label.
	ActionPreserve ActionKind = "preserve"
	// ActionRegister mints a fresh identity for an unowned path.
	ActionRegister ActionKind = "register"
	// ActionReplace retires the owning identity and mints a fresh one,
	// because the marker no longer identifies this path.
	ActionReplace ActionKind = "replace"
	// ActionRetire ends ownership of a path whose directory is gone.
	ActionRetire ActionKind = "retire"
	// ActionBlock keeps an identity but withholds execution because its
	// source cannot currently be trusted.
	ActionBlock ActionKind = "block"
	// ActionSuppressed reports a path whose identity was explicitly deleted.
	ActionSuppressed ActionKind = "suppressed"
	// ActionInvalid reports a path that cannot register yet.
	ActionInvalid ActionKind = "invalid"
	// ActionUnchanged is a no-op recorded for diagnostics.
	ActionUnchanged ActionKind = "unchanged"
)

// Action is one decision produced by reconciliation. It is data only: the
// planner never touches the filesystem or the database.
type Action struct {
	Kind ActionKind
	Path string
	// Owner is the instance that currently owns Path, when there is one.
	Owner ID
	// NewID is the freshly minted identity for Register and Replace.
	NewID ID
	// Name is the observed label to record.
	Name string
	// Reason explains refusals and blocks, and is written to the operation
	// journal for the mutations.
	Reason string
	// expectedMarker and expectedMarkerID record the marker observation this
	// decision was based on, so a journaled registration can verify its
	// precondition during recovery instead of overwriting a marker that
	// changed underneath it.
	expectedMarker   MarkerState
	expectedMarkerID ID
}

// Plan is the complete set of reconciliation decisions for one scan.
type Plan struct {
	Actions []Action
	// Complete mirrors the scan. An incomplete plan contains blocks only:
	// no registration, no retirement and no marker writes.
	Complete bool
	Errors   []string
}

// HasChanges reports whether the plan mutates the registry or the filesystem.
func (p Plan) HasChanges() bool {
	for _, a := range p.Actions {
		switch a.Kind {
		case ActionPreserve, ActionRegister, ActionReplace, ActionRetire:
			return true
		}
	}
	return false
}

// BuildPlan diffs a scan against the registry.
//
// The planner is pure and deterministic: given the same scan, registry and
// mint function it produces the same plan. It never adopts a marker into an
// existing identity, which is what makes a copied marker harmless. Mint is
// injected so tests can produce stable identifiers.
func BuildPlan(scan Scan, instances []Instance, paths []PathRecord, mint func() (ID, error)) (Plan, error) {
	if mint == nil {
		mint = Mint
	}

	instByID := make(map[ID]Instance, len(instances))
	for _, inst := range instances {
		instByID[inst.ID] = inst
	}

	pathByPath := make(map[string]PathRecord, len(paths))
	for _, rec := range paths {
		pathByPath[rec.CanonicalPath] = rec
	}

	observed := make(map[string]Observation, len(scan.Observations))
	order := make([]string, 0, len(scan.Observations))
	for _, obs := range scan.Observations {
		if _, seen := observed[obs.Path]; !seen {
			order = append(order, obs.Path)
		}
		observed[obs.Path] = obs
	}
	// Known paths take part even when the walk did not reach them.
	for path := range pathByPath {
		if _, seen := observed[path]; !seen {
			order = append(order, path)
		}
	}
	sort.Strings(order)

	plan := Plan{Complete: scan.Complete, Errors: append([]string(nil), scan.Errors...)}

	if !scan.Complete {
		// A failed walk proves nothing. Keep the registry exactly as it is and
		// withhold execution from anything whose source we could not verify.
		for _, path := range order {
			rec, owned := pathByPath[path]
			if !owned || rec.OwnerID.IsZero() {
				continue
			}
			if inst, ok := instByID[rec.OwnerID]; ok && inst.Status.AcceptsWork() {
				obs := observed[path]
				plan.Actions = append(plan.Actions, Action{
					Kind:   ActionBlock,
					Path:   path,
					Owner:  rec.OwnerID,
					Name:   obs.Name,
					Reason: "integrations scan was incomplete; source cannot be verified",
				})
			}
		}
		return plan, nil
	}

	for _, path := range order {
		obs := observed[path]
		rec, owned := pathByPath[path]

		var (
			owner    Instance
			hasOwner bool
		)
		if owned && !rec.OwnerID.IsZero() {
			owner, hasOwner = instByID[rec.OwnerID]
			if !hasOwner {
				// A dangling owner is a registry bug; refuse to guess.
				plan.Actions = append(plan.Actions, Action{
					Kind:   ActionBlock,
					Path:   path,
					Owner:  rec.OwnerID,
					Reason: fmt.Sprintf("path owner %s is not a registered instance", rec.OwnerID),
				})
				continue
			}
		}

		// A directory that is definitively gone retires its owner. This is the
		// only observation that ends an identity without a replacement.
		if hasOwner && !obs.Exists {
			if obs.ScanError != "" {
				plan.Actions = append(plan.Actions, Action{
					Kind:   ActionBlock,
					Path:   path,
					Owner:  owner.ID,
					Name:   owner.Name,
					Reason: "source could not be read: " + obs.ScanError,
				})
				continue
			}
			plan.Actions = append(plan.Actions, Action{
				Kind:   ActionRetire,
				Path:   path,
				Owner:  owner.ID,
				Name:   owner.Name,
				Reason: "source directory is gone",
			})
			continue
		}

		if !obs.Exists {
			// Unowned and absent: nothing to do.
			continue
		}

		if obs.ScanError != "" {
			kind := ActionInvalid
			if hasOwner {
				kind = ActionBlock
			}
			plan.Actions = append(plan.Actions, Action{
				Kind:   kind,
				Path:   path,
				Owner:  ownerOrZero(hasOwner, owner),
				Name:   obs.Name,
				Reason: "source could not be read: " + obs.ScanError,
			})
			continue
		}

		// A marker we cannot trust freezes the path: no reassignment, no
		// registration, until an operator repairs or resets it.
		if obs.Marker == MarkerUnsafe || obs.Marker == MarkerMalformed {
			kind := ActionInvalid
			if hasOwner {
				kind = ActionBlock
			}
			plan.Actions = append(plan.Actions, Action{
				Kind:   kind,
				Path:   path,
				Owner:  ownerOrZero(hasOwner, owner),
				Name:   obs.Name,
				Reason: "marker cannot be trusted: " + obs.MarkerError,
			})
			continue
		}

		switch {
		case hasOwner && obs.Marker == MarkerValid && obs.MarkerID == owner.ID:
			// The marker identifies the owner. Identity survives; only the
			// label and the execution gate can change.
			if err := validateObserved(obs); err != nil {
				plan.Actions = append(plan.Actions, Action{
					Kind:   ActionBlock,
					Path:   path,
					Owner:  owner.ID,
					Name:   obs.Name,
					Reason: err.Error(),
				})
				continue
			}
			plan.Actions = append(plan.Actions, Action{
				Kind:  ActionPreserve,
				Path:  path,
				Owner: owner.ID,
				Name:  obs.Name,
			})

		case hasOwner:
			// The marker is missing or names something else. The old identity
			// loses the path in every case; a copied marker never grants its
			// holder the owner's state.
			newID, err := mint()
			if err != nil {
				return Plan{}, err
			}
			if err := validateObserved(obs); err != nil {
				// The old registration can no longer claim the path, but a
				// replacement cannot be registered either. Retire the old
				// identity and defer the replacement.
				plan.Actions = append(plan.Actions, Action{
					Kind:   ActionRetire,
					Path:   path,
					Owner:  owner.ID,
					Name:   owner.Name,
					Reason: "source was replaced while the manifest was invalid",
				})
				plan.Actions = append(plan.Actions, Action{
					Kind:   ActionBlock,
					Path:   path,
					Owner:  owner.ID,
					NewID:  newID,
					Name:   obs.Name,
					Reason: "replacement deferred until the manifest is valid: " + err.Error(),
				})
				continue
			}
			plan.Actions = append(plan.Actions, Action{
				Kind:             ActionReplace,
				Path:             path,
				Owner:            owner.ID,
				NewID:            newID,
				Name:             obs.Name,
				Reason:           "source was replaced (marker does not match the registered identity)",
				expectedMarker:   obs.Marker,
				expectedMarkerID: obs.MarkerID,
			})

		default:
			// No current owner.
			if rec.Suppressed {
				plan.Actions = append(plan.Actions, Action{
					Kind:   ActionSuppressed,
					Path:   path,
					Name:   obs.Name,
					Reason: rec.SuppressionReason,
				})
				continue
			}
			if err := validateObserved(obs); err != nil {
				plan.Actions = append(plan.Actions, Action{
					Kind:   ActionInvalid,
					Path:   path,
					Name:   obs.Name,
					Reason: err.Error(),
				})
				continue
			}
			newID, err := mint()
			if err != nil {
				return Plan{}, err
			}
			plan.Actions = append(plan.Actions, Action{
				Kind:             ActionRegister,
				Path:             path,
				NewID:            newID,
				Name:             obs.Name,
				expectedMarker:   obs.Marker,
				expectedMarkerID: obs.MarkerID,
			})
		}
	}

	return plan, nil
}

func ownerOrZero(has bool, inst Instance) ID {
	if !has {
		return ""
	}
	return inst.ID
}

// validateObserved rejects a directory that cannot be registered yet.
func validateObserved(obs Observation) error {
	if !obs.ManifestPresent {
		return fmt.Errorf("no %s in %s", ManifestFileName, obs.Path)
	}
	if !obs.ManifestValid {
		if obs.ManifestError != "" {
			return fmt.Errorf("%s", obs.ManifestError)
		}
		return fmt.Errorf("manifest in %s is invalid", obs.Path)
	}
	if obs.Name == "" {
		return fmt.Errorf("manifest in %s declares no name", obs.Path)
	}
	return nil
}
