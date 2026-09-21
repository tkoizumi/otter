package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

// PurgeFunc removes the durable artifacts an instance owns: state, run history
// and logs, queue rows, webhook tokens, releases and prepared environments.
// Identity owns the registry, not those stores, so the daemon supplies this.
type PurgeFunc func(ctx context.Context, inst Instance) error

// ReconcileResult summarizes what applying a plan changed.
type ReconcileResult struct {
	Registered []ID
	Preserved  []ID
	Replaced   []ID
	Retired    []ID
	// Blocked names instances whose execution must be withheld until their
	// source verifies again. It is an execution gate, not retirement.
	Blocked []string
	// Suppressed, Invalid and Errors are diagnostics.
	Suppressed []string
	Invalid    []string
	Errors     []string
}

// Service applies reconciliation plans and performs explicit lifecycle
// operations against the registry and the filesystem.
//
// Every mutation that spans SQLite and the filesystem is journaled: intent is
// persisted first, the filesystem change happens second, and the registry
// commit is last. Recovery replays the journal, so a crash never leaves an
// instance half-registered or two identities claiming one path.
type Service struct {
	store *Store
	root  string
	mu    sync.Mutex
}

// NewService builds a service over a registry rooted at root.
//
// The root is canonicalized so containment checks compare like with like: on
// platforms where a temporary directory is reached through a symlink, the
// canonical root and the canonical source path must still match.
func NewService(store *Store, root string) *Service {
	if canonical, err := Canonical(root); err == nil {
		root = canonical
	}
	return &Service{store: store, root: root}
}

// Store exposes the underlying registry.
func (s *Service) Store() *Store { return s.store }

// Root reports the canonical integrations root.
func (s *Service) Root() string { return s.root }

// Reconcile builds and applies a plan for a scan.
func (s *Service) Reconcile(ctx context.Context, scan Scan) (ReconcileResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	instances, err := s.store.Instances(ctx)
	if err != nil {
		return ReconcileResult{}, err
	}
	paths, err := s.store.Paths(ctx)
	if err != nil {
		return ReconcileResult{}, err
	}
	plan, err := BuildPlan(scan, instances, paths, s.mintUnique(ctx))
	if err != nil {
		return ReconcileResult{}, err
	}
	return s.apply(ctx, plan)
}

// ApplyPlan applies a precomputed plan. It takes the same lock as Reconcile.
func (s *Service) ApplyPlan(ctx context.Context, plan Plan) (ReconcileResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apply(ctx, plan)
}

func (s *Service) apply(ctx context.Context, plan Plan) (ReconcileResult, error) {
	result := ReconcileResult{Errors: append([]string(nil), plan.Errors...)}

	for _, action := range plan.Actions {
		switch action.Kind {
		case ActionPreserve:
			if err := s.store.UpdateObservation(ctx, action.Owner, action.Name); err != nil {
				return result, err
			}
			result.Preserved = append(result.Preserved, action.Owner)

		case ActionRegister:
			if err := s.applyRegistration(ctx, action, ""); err != nil {
				return result, err
			}
			result.Registered = append(result.Registered, action.NewID)

		case ActionReplace:
			if err := s.applyRegistration(ctx, action, action.Owner); err != nil {
				return result, err
			}
			result.Retired = append(result.Retired, action.Owner)
			result.Replaced = append(result.Replaced, action.NewID)

		case ActionRetire:
			if err := s.retire(ctx, action.Owner, action.Reason); err != nil {
				return result, err
			}
			result.Retired = append(result.Retired, action.Owner)

		case ActionBlock:
			result.Blocked = append(result.Blocked, action.Path)

		case ActionSuppressed:
			result.Suppressed = append(result.Suppressed, action.Path)

		case ActionInvalid:
			result.Invalid = append(result.Invalid, action.Path)

		case ActionUnchanged:
		}
	}
	return result, nil
}

// applyRegistration journals and performs a registration: a fresh marker and a
// new active instance owning the path. oldOwner, when set, is retired first.
func (s *Service) applyRegistration(ctx context.Context, action Action, oldOwner ID) error {
	if action.NewID.IsZero() {
		return errors.New("identity: registration without an allocated id")
	}
	payload, err := json.Marshal(registerPayload{
		Name:                action.Name,
		NewID:               action.NewID.String(),
		OldOwner:            oldOwner.String(),
		ExpectedMarkerState: string(action.expectedMarker),
		ExpectedMarkerID:    action.expectedMarkerID.String(),
		Reason:              action.Reason,
	})
	if err != nil {
		return fmt.Errorf("identity: encode register payload: %w", err)
	}

	op := Operation{
		ID:         newOperationID(),
		Kind:       OpRegister,
		Phase:      PhasePlanned,
		InstanceID: action.NewID,
		ToPath:     action.Path,
		Data:       payload,
	}
	if err := s.store.CreateOperation(ctx, op); err != nil {
		return err
	}

	if oldOwner != "" {
		if err := s.retire(ctx, oldOwner, action.Reason); err != nil {
			return err
		}
	}

	if err := s.writeMarkerAndCommit(ctx, op); err != nil {
		// Leave the journal entry in place: recovery retries it, and a blocked
		// operation is visible to an operator.
		return err
	}
	return s.store.DeleteOperation(ctx, op.ID)
}

// writeMarkerAndCommit performs the filesystem half of a registration and then
// commits the registry half. It is idempotent so recovery can re-run it.
func (s *Service) writeMarkerAndCommit(ctx context.Context, op Operation) error {
	var payload registerPayload
	if err := json.Unmarshal(op.Data, &payload); err != nil {
		return fmt.Errorf("identity: decode register payload: %w", err)
	}
	newID, err := Parse(payload.NewID)
	if err != nil {
		return fmt.Errorf("identity: journaled id: %w", err)
	}

	currentState, currentID := observeMarker(op.ToPath)
	expected := MarkerState(payload.ExpectedMarkerState)
	switch {
	case currentState == MarkerValid && currentID == newID:
		// Already written; fall through to the commit.
	case currentState == expected && currentID == MustParseOrZero(payload.ExpectedMarkerID):
		if err := WriteMarker(op.ToPath, newID); err != nil {
			return fmt.Errorf("identity: write marker for %s: %w", op.ToPath, err)
		}
	default:
		// The tree changed underneath us. Do not overwrite an unexpected
		// marker; leave the operation blocked and visible.
		op.Phase = PhaseBlocked
		_ = s.store.UpdateOperation(ctx, op)
		return fmt.Errorf("identity: marker at %s changed during registration; refusing to overwrite", op.ToPath)
	}

	if err := s.commitInstance(ctx, newID, payload.Name, op.ToPath); err != nil {
		return err
	}
	op.Phase = PhaseDone
	return s.store.UpdateOperation(ctx, op)
}

// commitInstance is the idempotent registry half of a registration.
func (s *Service) commitInstance(ctx context.Context, id ID, name, path string) error {
	if _, err := s.store.Instance(ctx, id); err == nil {
		return s.store.ReservePath(ctx, path, id, "")
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	return s.store.CreateInstance(ctx, Instance{
		ID:            id,
		Name:          name,
		CanonicalPath: path,
		Status:        StatusActive,
		Generation:    1,
		CreatedAt:     time.Now().UTC(),
	})
}

// retire ends an instance's authority and releases its path.
func (s *Service) retire(ctx context.Context, id ID, reason string) error {
	if id == "" {
		return nil
	}
	inst, err := s.store.Instance(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if inst.Status == StatusRetired || inst.Status == StatusDeleted {
		return nil
	}
	if err := s.store.ReleasePath(ctx, inst.CanonicalPath); err != nil {
		return err
	}
	if _, err := s.store.BumpGeneration(ctx, id); err != nil {
		return err
	}
	return s.store.SetStatus(ctx, id, StatusRetired, reason)
}

// ---------------------------------------------------------------- explicit ops

// Register explicitly registers a source directory.
//
// It is idempotent when the path already has a matching active owner. It never
// adopts a supplied marker into an existing identity: an unowned or suppressed
// path gets a fresh id, and an actively owned path whose marker does not match
// must be reset or moved instead.
func (s *Service) Register(ctx context.Context, path, name string) (Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	canonical, err := Canonical(path)
	if err != nil {
		return Instance{}, err
	}
	if err := s.checkWithinRoot(canonical); err != nil {
		return Instance{}, err
	}

	rec, found, err := s.store.PathRecord(ctx, canonical)
	if err != nil {
		return Instance{}, err
	}
	if found && !rec.OwnerID.IsZero() {
		inst, err := s.store.Instance(ctx, rec.OwnerID)
		if err != nil {
			return Instance{}, err
		}
		marker, merr := ReadMarker(canonical)
		if merr == nil && marker == inst.ID {
			if err := s.store.UpdateObservation(ctx, inst.ID, name); err != nil {
				return Instance{}, err
			}
			return inst, nil
		}
		return Instance{}, fmt.Errorf("identity: %s is already owned by %s with a different marker; use reset", canonical, inst.ID)
	}

	if found && rec.Suppressed {
		if err := s.store.ClearSuppression(ctx, canonical); err != nil {
			return Instance{}, err
		}
	}

	markerState, markerID := observeMarker(canonical)
	newID, err := s.mintUnique(ctx)()
	if err != nil {
		return Instance{}, err
	}
	action := Action{
		Kind:             ActionRegister,
		Path:             canonical,
		NewID:            newID,
		Name:             name,
		expectedMarker:   markerState,
		expectedMarkerID: markerID,
	}
	if err := s.applyRegistration(ctx, action, ""); err != nil {
		return Instance{}, err
	}
	return s.store.Instance(ctx, newID)
}

// Reset retires an instance's identity and registers a fresh one at the same
// path, preserving the old data for explicit inspection or deletion.
func (s *Service) Reset(ctx context.Context, id ID, name string) (Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	inst, err := s.store.Instance(ctx, id)
	if err != nil {
		return Instance{}, err
	}
	if inst.CanonicalPath == "" {
		return Instance{}, fmt.Errorf("identity: %s has no source path to reset", id)
	}

	op := Operation{
		ID:         newOperationID(),
		Kind:       OpReset,
		Phase:      PhasePlanned,
		InstanceID: id,
		FromPath:   inst.CanonicalPath,
	}
	if err := s.store.CreateOperation(ctx, op); err != nil {
		return Instance{}, err
	}

	newID, err := s.mintUnique(ctx)()
	if err != nil {
		return Instance{}, err
	}
	markerState, markerID := observeMarker(inst.CanonicalPath)

	if err := s.retire(ctx, id, "reset by operator"); err != nil {
		return Instance{}, err
	}
	if err := s.applyRegistration(ctx, Action{
		Kind:             ActionRegister,
		Path:             inst.CanonicalPath,
		NewID:            newID,
		Name:             name,
		expectedMarker:   markerState,
		expectedMarkerID: markerID,
	}, ""); err != nil {
		return Instance{}, err
	}
	op.Phase = PhaseDone
	if err := s.store.UpdateOperation(ctx, op); err != nil {
		return Instance{}, err
	}
	return s.store.Instance(ctx, newID)
}

// Delete retires an instance and purges everything it owns. Source files are
// left in place; the path is suppressed so discovery does not immediately
// re-register it. The instance row survives as a tombstone so the id is never
// reused.
func (s *Service) Delete(ctx context.Context, id ID, purge PurgeFunc) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	inst, err := s.store.Instance(ctx, id)
	if err != nil {
		return err
	}
	if inst.Status == StatusDeleted {
		return nil
	}

	op := Operation{
		ID:         newOperationID(),
		Kind:       OpDelete,
		Phase:      PhasePlanned,
		InstanceID: id,
		FromPath:   inst.CanonicalPath,
	}
	if err := s.store.CreateOperation(ctx, op); err != nil {
		return err
	}

	if err := s.store.SetStatus(ctx, id, StatusDeleting, "delete requested"); err != nil {
		return err
	}
	if _, err := s.store.BumpGeneration(ctx, id); err != nil {
		return err
	}
	if err := s.store.ReleasePath(ctx, inst.CanonicalPath); err != nil {
		return err
	}

	op.Phase = PhaseReserved
	if err := s.store.UpdateOperation(ctx, op); err != nil {
		return err
	}

	if purge != nil {
		if err := purge(ctx, inst); err != nil {
			// Remain deleting so a retry can finish; the journal makes that
			// resumable after a restart.
			return fmt.Errorf("identity: purge %s: %w", id, err)
		}
	}

	if inst.CanonicalPath != "" {
		if err := s.store.SuppressPath(ctx, inst.CanonicalPath, id, "deleted by operator"); err != nil {
			return err
		}
	}
	if err := s.store.SetStatus(ctx, id, StatusDeleted, "deleted by operator"); err != nil {
		return err
	}
	op.Phase = PhaseDone
	if err := s.store.UpdateOperation(ctx, op); err != nil {
		return err
	}
	return s.store.DeleteOperation(ctx, op.ID)
}

// Move preserves an identity across a same-filesystem directory rename. The
// destination must be inside the root, unowned, and must not contain or be
// contained by another registered integration.
func (s *Service) Move(ctx context.Context, id ID, destination string) (Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	inst, err := s.store.Instance(ctx, id)
	if err != nil {
		return Instance{}, err
	}
	if inst.Status != StatusActive {
		return Instance{}, fmt.Errorf("identity: %s is %s, not active", id, inst.Status)
	}
	if inst.CanonicalPath == "" {
		return Instance{}, fmt.Errorf("identity: %s has no source path to move", id)
	}

	from := inst.CanonicalPath
	to, err := Canonical(destination)
	if err != nil {
		return Instance{}, err
	}
	if err := s.checkWithinRoot(to); err != nil {
		return Instance{}, err
	}
	if from == to {
		return inst, nil
	}

	marker, err := ReadMarker(from)
	if err != nil {
		return Instance{}, fmt.Errorf("identity: source marker for %s: %w", from, err)
	}
	if marker != id {
		return Instance{}, fmt.Errorf("identity: marker at %s does not match %s", from, id)
	}

	if _, exists, err := s.store.PathRecord(ctx, to); err != nil {
		return Instance{}, err
	} else if exists {
		return Instance{}, fmt.Errorf("%w: destination %s already has a registry row", ErrPathOwned, to)
	}
	if err := s.rejectNested(ctx, id, to); err != nil {
		return Instance{}, err
	}
	if _, err := os.Lstat(to); err == nil {
		return Instance{}, fmt.Errorf("identity: destination %s already exists", to)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Instance{}, err
	}

	op := Operation{
		ID:         newOperationID(),
		Kind:       OpMove,
		Phase:      PhasePlanned,
		InstanceID: id,
		FromPath:   from,
		ToPath:     to,
	}
	if err := s.store.CreateOperation(ctx, op); err != nil {
		return Instance{}, err
	}
	if err := s.store.ReservePath(ctx, to, id, "move destination"); err != nil {
		return Instance{}, err
	}
	if _, err := s.store.BumpGeneration(ctx, id); err != nil {
		return Instance{}, err
	}

	op.Phase = PhaseReserved
	if err := s.store.UpdateOperation(ctx, op); err != nil {
		return Instance{}, err
	}

	if err := os.Rename(from, to); err != nil {
		return Instance{}, fmt.Errorf("identity: move %s to %s: %w", from, to, err)
	}

	if err := s.store.ReleasePath(ctx, from); err != nil {
		return Instance{}, err
	}
	if err := s.store.SetCanonicalPath(ctx, id, to); err != nil {
		return Instance{}, err
	}
	op.Phase = PhaseDone
	if err := s.store.UpdateOperation(ctx, op); err != nil {
		return Instance{}, err
	}
	if err := s.store.DeleteOperation(ctx, op.ID); err != nil {
		return Instance{}, err
	}
	return s.store.Instance(ctx, id)
}

// rejectNested refuses a move that would nest registrations inside one another.
func (s *Service) rejectNested(ctx context.Context, id ID, to string) error {
	instances, err := s.store.Instances(ctx)
	if err != nil {
		return err
	}
	for _, other := range instances {
		if other.ID == id || other.Status != StatusActive || other.CanonicalPath == "" {
			continue
		}
		if IsNested(to, other.CanonicalPath) || IsNested(other.CanonicalPath, to) {
			return fmt.Errorf("identity: %s would nest %s and %s", to, id, other.ID)
		}
	}
	return nil
}

// checkWithinRoot refuses a path outside the configured integrations root.
func (s *Service) checkWithinRoot(path string) error {
	if s.root == "" {
		return nil
	}
	if !Within(s.root, path) {
		return fmt.Errorf("identity: %s is outside the integrations root %s", path, s.root)
	}
	return nil
}

// mintUnique mints identifiers that no existing row, including tombstones,
// already uses.
func (s *Service) mintUnique(ctx context.Context) func() (ID, error) {
	return func() (ID, error) {
		for attempt := 0; attempt < 8; attempt++ {
			id, err := Mint()
			if err != nil {
				return "", err
			}
			if _, err := s.store.Instance(ctx, id); errors.Is(err, ErrNotFound) {
				return id, nil
			} else if err != nil {
				return "", err
			}
		}
		return "", errors.New("identity: could not mint a unique id")
	}
}

// ---------------------------------------------------------------- recovery

// Recover replays unfinished journal entries. It runs before normal
// reconciliation so a crash midway through a mutation converges on one
// identity rather than minting a second one.
func (s *Service) Recover(ctx context.Context) (ReconcileResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ops, err := s.store.UnfinishedOperations(ctx)
	if err != nil {
		return ReconcileResult{}, err
	}
	var result ReconcileResult
	for _, op := range ops {
		switch op.Kind {
		case OpRegister:
			if op.Phase == PhaseBlocked {
				result.Blocked = append(result.Blocked, op.ToPath)
				continue
			}
			if err := s.writeMarkerAndCommit(ctx, op); err != nil {
				result.Errors = append(result.Errors, err.Error())
				continue
			}
			if err := s.store.DeleteOperation(ctx, op.ID); err != nil {
				return result, err
			}
			if id, err := Parse(op.InstanceID.String()); err == nil {
				result.Registered = append(result.Registered, id)
			}
		case OpDelete:
			inst, err := s.store.Instance(ctx, op.InstanceID)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					_ = s.store.DeleteOperation(ctx, op.ID)
					continue
				}
				return result, err
			}
			if inst.CanonicalPath != "" {
				if err := s.store.SuppressPath(ctx, inst.CanonicalPath, inst.ID, "deleted by operator"); err != nil {
					return result, err
				}
			}
			if err := s.store.SetStatus(ctx, inst.ID, StatusDeleted, "deleted by operator"); err != nil {
				return result, err
			}
			if err := s.store.DeleteOperation(ctx, op.ID); err != nil {
				return result, err
			}
		case OpReset:
			// A reset that did not finish leaves the old identity retired and
			// possibly a registration journaled separately; nothing to do here
			// beyond clearing the marker.
			op.Phase = PhaseDone
			if err := s.store.UpdateOperation(ctx, op); err != nil {
				return result, err
			}
		case OpMove:
			if err := s.recoverMove(ctx, op); err != nil {
				result.Errors = append(result.Errors, err.Error())
			}
		}
	}
	return result, nil
}

func (s *Service) recoverMove(ctx context.Context, op Operation) error {
	_, fromErr := os.Lstat(op.FromPath)
	_, toErr := os.Lstat(op.ToPath)
	switch {
	case toErr == nil && errors.Is(fromErr, os.ErrNotExist):
		// The rename completed. Finish the binding.
		if err := s.store.ReleasePath(ctx, op.FromPath); err != nil {
			return err
		}
		if err := s.store.SetCanonicalPath(ctx, op.InstanceID, op.ToPath); err != nil {
			return err
		}
	case fromErr == nil && errors.Is(toErr, os.ErrNotExist):
		// The rename never happened. Release the reserved destination.
		if err := s.store.ReleasePath(ctx, op.ToPath); err != nil {
			return err
		}
	case fromErr == nil && toErr == nil:
		return fmt.Errorf("identity: move %s is ambiguous: both %s and %s exist", op.ID, op.FromPath, op.ToPath)
	default:
		return fmt.Errorf("identity: move %s is ambiguous: neither %s nor %s exists", op.ID, op.FromPath, op.ToPath)
	}
	op.Phase = PhaseDone
	if err := s.store.UpdateOperation(ctx, op); err != nil {
		return err
	}
	return s.store.DeleteOperation(ctx, op.ID)
}

// ---------------------------------------------------------------- helpers

type registerPayload struct {
	Name                string `json:"name"`
	NewID               string `json:"new_id"`
	OldOwner            string `json:"old_owner,omitempty"`
	ExpectedMarkerState string `json:"expected_marker_state"`
	ExpectedMarkerID    string `json:"expected_marker_id,omitempty"`
	Reason              string `json:"reason,omitempty"`
}

func newOperationID() string { return uuid.NewString() }

func observeMarker(dir string) (MarkerState, ID) {
	id, err := ReadMarker(dir)
	switch {
	case err == nil:
		return MarkerValid, id
	case errors.Is(err, ErrNoMarker):
		return MarkerNone, ""
	case errors.Is(err, ErrUnsafeMarker):
		return MarkerUnsafe, ""
	default:
		return MarkerMalformed, ""
	}
}

// MustParseOrZero parses a stored id, returning the zero id when it is empty.
func MustParseOrZero(raw string) ID {
	if raw == "" {
		return ""
	}
	id, err := Parse(raw)
	if err != nil {
		return ""
	}
	return id
}
