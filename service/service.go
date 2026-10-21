// Package service implements ModelDock's use cases on top of the store. Both
// the REST and GraphQL APIs call it, so authorization and the version state
// machine are enforced in exactly one place.
//
// Every mutating method runs in a single database transaction that (1) locks
// the model row, (2) validates the transition, (3) writes the new state and
// (4) appends the audit event to the outbox. Kubernetes is never called from
// a request: the reconciler applies pending deployments, so a crash between
// commit and apply is harmless.
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wenxuewu0216/modeldock/auth"
	"github.com/wenxuewu0216/modeldock/domain"
	"github.com/wenxuewu0216/modeldock/events"
	"github.com/wenxuewu0216/modeldock/kube"
	"github.com/wenxuewu0216/modeldock/store"
)

// SystemActor is the audit actor for reconciler-initiated changes.
const SystemActor = "system:reconciler"

// Kicker wakes the reconciler (kube.Reconciler implements it).
type Kicker interface{ Kick() }

// Service is the application core.
type Service struct {
	Store   *store.Store
	Kicker  Kicker
	Metrics *Metrics
	// AutoRollback makes a failed rollout automatically roll back to the
	// previous version that was ready in the same environment.
	AutoRollback bool
}

// New returns a Service. kicker may be nil.
func New(s *store.Store, kicker Kicker, m *Metrics, autoRollback bool) *Service {
	if m == nil {
		m = NewMetrics(nil)
	}
	return &Service{Store: s, Kicker: kicker, Metrics: m, AutoRollback: autoRollback}
}

func (s *Service) kick() {
	if s.Kicker != nil {
		s.Kicker.Kick()
	}
}

func event(typ string, u string, model, version string, env domain.Environment, depID string, data any) *events.Event {
	ev := events.New(typ, u, data)
	ev.Model, ev.Version, ev.Environment, ev.DeploymentID = model, version, string(env), depID
	return &ev
}

// transition applies ev to v inside tx (compare-and-set on the stored state).
func transition(ctx context.Context, tx *store.Tx, v *domain.ModelVersion, ev domain.VersionEvent) error {
	to, err := domain.Transition(v.State, ev)
	if err != nil {
		return fmt.Errorf("version %s/%s: %w", v.ModelName, v.Version, err)
	}
	if err := tx.SetVersionState(ctx, v.ID, v.State, to); err != nil {
		return err
	}
	v.State = to
	return nil
}

// --- registry ---

// CreateModel registers a new model.
func (s *Service) CreateModel(ctx context.Context, u domain.User, name, description string) (domain.Model, error) {
	if err := auth.Require(u, auth.PermRegister); err != nil {
		return domain.Model{}, err
	}
	if err := domain.ValidateModelName(name); err != nil {
		return domain.Model{}, err
	}
	m := domain.Model{Name: name, Description: description, CreatedBy: u.ID}
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		if err := tx.CreateModel(ctx, &m); err != nil {
			return err
		}
		return tx.AppendEvent(ctx, event(events.TypeModelRegistered, u.ID, m.Name, "", "", "",
			map[string]string{"description": description}))
	})
	if err != nil {
		return domain.Model{}, fmt.Errorf("service: create model %s: %w", name, err)
	}
	return m, nil
}

// RegisterVersion registers an immutable model version.
func (s *Service) RegisterVersion(ctx context.Context, u domain.User, model string, v domain.ModelVersion) (domain.ModelVersion, error) {
	if err := auth.Require(u, auth.PermRegister); err != nil {
		return domain.ModelVersion{}, err
	}
	v.State = domain.StateRegistered
	v.CreatedBy = u.ID
	if err := v.Validate(); err != nil {
		return domain.ModelVersion{}, err
	}
	if err := kube.ValidateResources(v.Resources); err != nil {
		return domain.ModelVersion{}, err
	}
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		m, err := tx.LockModel(ctx, model)
		if err != nil {
			return err
		}
		if err := tx.CreateVersion(ctx, m, &v); err != nil {
			return err
		}
		return tx.AppendEvent(ctx, event(events.TypeVersionRegistered, u.ID, m.Name, v.Version, "", "",
			map[string]any{"image": v.Image, "resources": v.Resources, "scaling": v.Scaling, "metadata": v.Metadata}))
	})
	if err != nil {
		return domain.ModelVersion{}, fmt.Errorf("service: register %s/%s: %w", model, v.Version, err)
	}
	return v, nil
}

// --- deployment lifecycle ---

// displaceCurrent marks the current revision of (model, env), if it is
// still live or in flight, as replaced by a new revision.
func displaceCurrent(ctx context.Context, tx *store.Tx, modelID string, env domain.Environment, as domain.DeploymentStatus) (*domain.Deployment, error) {
	cur, err := tx.CurrentDeployment(ctx, modelID, env)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if cur.Status != domain.DeployReady && !cur.Status.InFlight() {
		return &cur, nil
	}
	upd, err := tx.UpdateDeployment(ctx, cur.ID, store.DeploymentUpdate{
		From: []domain.DeploymentStatus{domain.DeployPending, domain.DeployProgressing, domain.DeployReady},
		To:   as, FinishedAt: time.Now(),
	})
	if err != nil {
		return nil, err
	}
	return &upd, nil
}

// DeployToStaging creates a new staging revision for model/version.
func (s *Service) DeployToStaging(ctx context.Context, u domain.User, model, version string) (domain.Deployment, error) {
	if err := auth.Require(u, auth.PermDeploy); err != nil {
		return domain.Deployment{}, err
	}
	var d domain.Deployment
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		m, err := tx.LockModel(ctx, model)
		if err != nil {
			return err
		}
		v, err := tx.GetVersion(ctx, model, version)
		if err != nil {
			return err
		}
		if err := transition(ctx, tx, &v, domain.EvDeployStaging); err != nil {
			return err
		}
		if _, err := displaceCurrent(ctx, tx, m.ID, domain.EnvStaging, domain.DeploySuperseded); err != nil {
			return err
		}
		d = domain.Deployment{ModelID: m.ID, ModelName: m.Name, VersionID: v.ID, Version: v.Version,
			Environment: domain.EnvStaging, Kind: domain.KindDeploy, Status: domain.DeployPending, CreatedBy: u.ID}
		if err := tx.CreateDeployment(ctx, &d); err != nil {
			return err
		}
		return tx.AppendEvent(ctx, event(events.TypeDeployRequested, u.ID, m.Name, v.Version, d.Environment, d.ID,
			map[string]any{"revision": d.Revision, "image": v.Image}))
	})
	if err != nil {
		return domain.Deployment{}, fmt.Errorf("service: deploy %s/%s to staging: %w", model, version, err)
	}
	s.kick()
	return d, nil
}

// RequestPromotion asks for approval to promote a version that is running
// successfully in staging.
func (s *Service) RequestPromotion(ctx context.Context, u domain.User, model, version, comment string) (domain.Approval, error) {
	if err := auth.Require(u, auth.PermDeploy); err != nil {
		return domain.Approval{}, err
	}
	var a domain.Approval
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		if _, err := tx.LockModel(ctx, model); err != nil {
			return err
		}
		v, err := tx.GetVersion(ctx, model, version)
		if err != nil {
			return err
		}
		ok, err := tx.WasReadyIn(ctx, v.ID, domain.EnvStaging)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: version %s has never been ready in staging", domain.ErrConflict, version)
		}
		if err := transition(ctx, tx, &v, domain.EvRequestPromotion); err != nil {
			return err
		}
		a = domain.Approval{VersionID: v.ID, ModelName: model, Version: version, RequestedBy: u.ID, Comment: comment}
		if err := tx.CreateApproval(ctx, &a); err != nil {
			return err
		}
		return tx.AppendEvent(ctx, event(events.TypePromotionRequested, u.ID, model, version, domain.EnvProd, "",
			map[string]string{"approvalId": a.ID, "comment": comment}))
	})
	if err != nil {
		return domain.Approval{}, fmt.Errorf("service: request promotion of %s/%s: %w", model, version, err)
	}
	return a, nil
}

// Decide approves or rejects a pending promotion request.
func (s *Service) Decide(ctx context.Context, u domain.User, approvalID string, approve bool, comment string) (domain.Approval, error) {
	if err := auth.Require(u, auth.PermApprove); err != nil {
		return domain.Approval{}, err
	}
	status, vev, typ := domain.ApprovalRejected, domain.EvReject, events.TypeApprovalRejected
	if approve {
		status, vev, typ = domain.ApprovalApproved, domain.EvApprove, events.TypeApprovalApproved
	}
	var out domain.Approval
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		a, err := tx.GetApproval(ctx, approvalID)
		if err != nil {
			return err
		}
		if _, err := tx.LockModel(ctx, a.ModelName); err != nil {
			return err
		}
		if err := auth.CanDecide(u, a); err != nil {
			return err
		}
		v, err := tx.GetVersionByID(ctx, a.VersionID)
		if err != nil {
			return err
		}
		if err := transition(ctx, tx, &v, vev); err != nil {
			return err
		}
		if out, err = tx.DecideApproval(ctx, a.ID, status, u.ID, comment); err != nil {
			return err
		}
		return tx.AppendEvent(ctx, event(typ, u.ID, a.ModelName, a.Version, domain.EnvProd, "",
			map[string]string{"approvalId": a.ID, "requestedBy": a.RequestedBy, "comment": comment}))
	})
	if err != nil {
		return domain.Approval{}, fmt.Errorf("service: decide approval %s: %w", approvalID, err)
	}
	s.Metrics.Approvals.WithLabelValues(string(status)).Inc()
	return out, nil
}

// Promote rolls an approved version out to prod.
func (s *Service) Promote(ctx context.Context, u domain.User, model, version string) (domain.Deployment, error) {
	if err := auth.Require(u, auth.PermDeploy); err != nil {
		return domain.Deployment{}, err
	}
	var d domain.Deployment
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		m, err := tx.LockModel(ctx, model)
		if err != nil {
			return err
		}
		v, err := tx.GetVersion(ctx, model, version)
		if err != nil {
			return err
		}
		// Belt and braces: the state machine already requires "approved",
		// but also check the approval record itself.
		a, err := tx.LatestApproval(ctx, v.ID)
		if err != nil || a.Status != domain.ApprovalApproved {
			return fmt.Errorf("%w: %s/%s has no approved promotion request", domain.ErrConflict, model, version)
		}
		if err := transition(ctx, tx, &v, domain.EvPromote); err != nil {
			return err
		}
		prev, err := displaceCurrent(ctx, tx, m.ID, domain.EnvProd, domain.DeploySuperseded)
		if err != nil {
			return err
		}
		if prev != nil && prev.VersionID != v.ID {
			pv, err := tx.GetVersionByID(ctx, prev.VersionID)
			if err != nil {
				return err
			}
			if domain.CanTransition(pv.State, domain.EvSupersede) {
				if err := transition(ctx, tx, &pv, domain.EvSupersede); err != nil {
					return err
				}
			}
		}
		d = domain.Deployment{ModelID: m.ID, ModelName: m.Name, VersionID: v.ID, Version: v.Version,
			Environment: domain.EnvProd, Kind: domain.KindPromote, Status: domain.DeployPending, CreatedBy: u.ID}
		if err := tx.CreateDeployment(ctx, &d); err != nil {
			return err
		}
		return tx.AppendEvent(ctx, event(events.TypePromoteStarted, u.ID, m.Name, v.Version, d.Environment, d.ID,
			map[string]any{"revision": d.Revision, "approvalId": a.ID, "approvedBy": a.DecidedBy}))
	})
	if err != nil {
		return domain.Deployment{}, fmt.Errorf("service: promote %s/%s: %w", model, version, err)
	}
	s.Metrics.Promotions.WithLabelValues(model).Inc()
	s.kick()
	return d, nil
}

// Rollback creates a new revision in env running toVersion, or the previous
// version that was ready there if toVersion is empty.
func (s *Service) Rollback(ctx context.Context, u domain.User, model string, env domain.Environment, toVersion string) (domain.Deployment, error) {
	if err := auth.Require(u, auth.PermDeploy); err != nil {
		return domain.Deployment{}, err
	}
	if !env.Valid() {
		return domain.Deployment{}, fmt.Errorf("%w: unknown environment %q", domain.ErrInvalid, env)
	}
	var d domain.Deployment
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		m, err := tx.LockModel(ctx, model)
		if err != nil {
			return err
		}
		d, err = rollbackTx(ctx, tx, m, env, toVersion, u.ID, domain.KindRollback, "")
		return err
	})
	if err != nil {
		return domain.Deployment{}, fmt.Errorf("service: rollback %s in %s: %w", model, env, err)
	}
	s.Metrics.Rollbacks.WithLabelValues(string(env), "manual").Inc()
	s.kick()
	return d, nil
}

func rollbackTx(ctx context.Context, tx *store.Tx, m domain.Model, env domain.Environment, toVersion, actor string,
	kind domain.DeploymentKind, reason string) (domain.Deployment, error) {
	cur, err := tx.CurrentDeployment(ctx, m.ID, env)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.Deployment{}, fmt.Errorf("%w: %s has never been deployed to %s", domain.ErrConflict, m.Name, env)
		}
		return domain.Deployment{}, err
	}
	var target domain.ModelVersion
	if toVersion == "" {
		id, err := tx.PreviousGoodVersion(ctx, m.ID, env, cur.VersionID)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.Deployment{}, fmt.Errorf("%w: no previous version was ever ready in %s", domain.ErrConflict, env)
		}
		if err != nil {
			return domain.Deployment{}, err
		}
		if target, err = tx.GetVersionByID(ctx, id); err != nil {
			return domain.Deployment{}, err
		}
	} else {
		if target, err = tx.GetVersion(ctx, m.Name, toVersion); err != nil {
			return domain.Deployment{}, err
		}
	}
	if target.ID == cur.VersionID {
		return domain.Deployment{}, fmt.Errorf("%w: %s is already the current version in %s", domain.ErrConflict, target.Version, env)
	}
	ok, err := tx.WasReadyIn(ctx, target.ID, env)
	if err != nil {
		return domain.Deployment{}, err
	}
	if !ok {
		return domain.Deployment{}, fmt.Errorf("%w: version %s was never ready in %s; only known-good versions can be rolled back to",
			domain.ErrConflict, target.Version, env)
	}

	// Version states: the displaced version is rolled back from, the target
	// becomes live again. Prod requires the target to have been live in prod
	// (superseded/rolled_back); the state machine enforces that.
	curV, err := tx.GetVersionByID(ctx, cur.VersionID)
	if err != nil {
		return domain.Deployment{}, err
	}
	if domain.CanTransition(curV.State, domain.EvRollbackFrom) && (env == domain.EnvProd) == (curV.State == domain.StateProduction) {
		if err := transition(ctx, tx, &curV, domain.EvRollbackFrom); err != nil {
			return domain.Deployment{}, err
		}
	}
	if env == domain.EnvProd {
		if err := transition(ctx, tx, &target, domain.EvRollbackToProd); err != nil {
			return domain.Deployment{}, err
		}
	} else if domain.CanTransition(target.State, domain.EvRollbackToStaging) {
		if err := transition(ctx, tx, &target, domain.EvRollbackToStaging); err != nil {
			return domain.Deployment{}, err
		}
	}
	if _, err := displaceCurrent(ctx, tx, m.ID, env, domain.DeployRolledBack); err != nil {
		return domain.Deployment{}, err
	}
	d := domain.Deployment{ModelID: m.ID, ModelName: m.Name, VersionID: target.ID, Version: target.Version,
		Environment: env, Kind: kind, Status: domain.DeployPending, CreatedBy: actor, Reason: reason}
	if err := tx.CreateDeployment(ctx, &d); err != nil {
		return domain.Deployment{}, err
	}
	err = tx.AppendEvent(ctx, event(events.TypeRollbackStarted, actor, m.Name, target.Version, env, d.ID,
		map[string]any{"revision": d.Revision, "fromVersion": cur.Version, "fromDeploymentId": cur.ID,
			"kind": kind, "reason": reason}))
	return d, err
}

// --- reconciler callbacks (kube.Transitions) ---

var _ kube.Transitions = (*Service)(nil)

// MarkProgressing records that a pending deployment was applied.
func (s *Service) MarkProgressing(ctx context.Context, id string) error {
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		_, err := tx.UpdateDeployment(ctx, id, store.DeploymentUpdate{
			From: []domain.DeploymentStatus{domain.DeployPending}, To: domain.DeployProgressing, StartedAt: time.Now(),
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("service: mark %s progressing: %w", id, err)
	}
	return nil
}

// MarkReady records a completed rollout.
func (s *Service) MarkReady(ctx context.Context, id string) error {
	var d domain.Deployment
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		now := time.Now()
		var err error
		d, err = tx.UpdateDeployment(ctx, id, store.DeploymentUpdate{
			From: []domain.DeploymentStatus{domain.DeployProgressing}, To: domain.DeployReady, ReadyAt: now, FinishedAt: now,
		})
		if err != nil {
			return err
		}
		return tx.AppendEvent(ctx, event(events.TypeDeployReady, SystemActor, d.ModelName, d.Version, d.Environment, d.ID,
			map[string]any{"revision": d.Revision, "kind": d.Kind, "durationSeconds": now.Sub(d.CreatedAt).Seconds()}))
	})
	if err != nil {
		return fmt.Errorf("service: mark %s ready: %w", id, err)
	}
	s.Metrics.DeployDuration.WithLabelValues(string(d.Environment), string(d.Kind), "ready").
		Observe(d.ReadyAt.Sub(d.CreatedAt).Seconds())
	return nil
}

// MarkFailed records a failed rollout and, if enabled, starts an automatic
// rollback in the same transaction.
func (s *Service) MarkFailed(ctx context.Context, id, reason string) error {
	var d domain.Deployment
	var rb *domain.Deployment
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		cur, err := tx.GetDeployment(ctx, id)
		if err != nil {
			return err
		}
		m, err := tx.LockModel(ctx, cur.ModelName)
		if err != nil {
			return err
		}
		now := time.Now()
		d, err = tx.UpdateDeployment(ctx, id, store.DeploymentUpdate{
			From: []domain.DeploymentStatus{domain.DeployPending, domain.DeployProgressing},
			To:   domain.DeployFailed, Reason: reason, FinishedAt: now,
		})
		if err != nil {
			return err
		}
		v, err := tx.GetVersionByID(ctx, d.VersionID)
		if err != nil {
			return err
		}
		// Only fail the version if this environment is where it is live:
		// a failed staging redeploy must not mark a prod version failed.
		live := (d.Environment == domain.EnvProd && v.State == domain.StateProduction) ||
			(d.Environment == domain.EnvStaging && v.State == domain.StateStaging)
		if live {
			if err := transition(ctx, tx, &v, domain.EvFail); err != nil {
				return err
			}
		}
		if err := tx.AppendEvent(ctx, event(events.TypeDeployFailed, SystemActor, d.ModelName, d.Version, d.Environment, d.ID,
			map[string]any{"revision": d.Revision, "kind": d.Kind, "reason": reason})); err != nil {
			return err
		}
		if !s.AutoRollback || d.Kind == domain.KindAutoRollback {
			return nil // never auto-rollback a failed auto-rollback: no loops
		}
		r, err := rollbackTx(ctx, tx, m, d.Environment, "", SystemActor, domain.KindAutoRollback,
			"automatic rollback: revision "+fmt.Sprint(d.Revision)+" failed: "+reason)
		if errors.Is(err, domain.ErrConflict) {
			return nil // nothing known-good to roll back to
		}
		if err != nil {
			return err
		}
		rb = &r
		return nil
	})
	if err != nil {
		return fmt.Errorf("service: mark %s failed: %w", id, err)
	}
	s.Metrics.DeployDuration.WithLabelValues(string(d.Environment), string(d.Kind), "failed").
		Observe(d.FinishedAt.Sub(d.CreatedAt).Seconds())
	if rb != nil {
		s.Metrics.Rollbacks.WithLabelValues(string(rb.Environment), "auto").Inc()
		s.kick()
	}
	return nil
}

// ReportHealth records an inference-health change of a live deployment.
func (s *Service) ReportHealth(ctx context.Context, d domain.Deployment, health string, st kube.WorkloadStatus) error {
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		return tx.AppendEvent(ctx, event(events.TypeInferenceHealth, SystemActor, d.ModelName, d.Version, d.Environment, d.ID,
			events.HealthData{Status: health, ReadyReplicas: st.Ready, DesiredReplicas: st.Desired, AvailableReplicas: st.Available}))
	})
	if err != nil {
		return fmt.Errorf("service: report health for %s: %w", d.ID, err)
	}
	s.Metrics.HealthChanges.WithLabelValues(string(d.Environment), health).Inc()
	return nil
}
