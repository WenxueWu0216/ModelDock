package kube

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/wenxuewu0216/modeldock/domain"
)

// ReconcileStore is the read side the reconciler needs (store.Store).
type ReconcileStore interface {
	ListInFlight(ctx context.Context) ([]domain.Deployment, error)
	ListLive(ctx context.Context) ([]domain.Deployment, error)
	GetVersionByID(ctx context.Context, id string) (domain.ModelVersion, error)
	TryLeader(ctx context.Context, key int64) (release func(), ok bool, err error)
}

// Transitions are the state changes the reconciler requests. The service
// layer implements them transactionally (state change + audit event in one
// transaction), and handles auto-rollback inside MarkFailed.
type Transitions interface {
	MarkProgressing(ctx context.Context, deploymentID string) error
	MarkReady(ctx context.Context, deploymentID string) error
	MarkFailed(ctx context.Context, deploymentID, reason string) error
	ReportHealth(ctx context.Context, d domain.Deployment, health string, st WorkloadStatus) error
}

// ReconcilerLockKey elects the single active reconciler among replicas.
const ReconcilerLockKey int64 = 0x6d6f64656c02

// Reconciler converges in-flight deployments: it applies pending ones,
// watches progressing ones until they are ready, and fails any that miss the
// readiness deadline. It also emits inference-health changes for live
// deployments.
type Reconciler struct {
	Store       ReconcileStore
	Kube        Client
	Transitions Transitions
	// Timeout is how long a rollout may take to become ready.
	Timeout  time.Duration
	Interval time.Duration
	Log      *slog.Logger
	Now      func() time.Time

	kick       chan struct{}
	once       sync.Once
	mu         sync.Mutex
	lastHealth map[string]string // deployment ID -> last reported health
}

func (r *Reconciler) init() {
	r.once.Do(func() {
		r.kick = make(chan struct{}, 1)
		r.lastHealth = map[string]string{}
		if r.Now == nil {
			r.Now = time.Now
		}
		if r.Log == nil {
			r.Log = slog.Default()
		}
		if r.Timeout <= 0 {
			r.Timeout = 5 * time.Minute
		}
		if r.Interval <= 0 {
			r.Interval = 2 * time.Second
		}
	})
}

// Kick asks the reconciler to run soon (after the API records a new
// deployment) instead of waiting for the next tick. Never blocks.
func (r *Reconciler) Kick() {
	r.init()
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// DeadlineSeconds is the progress deadline put on Deployments: 90% of
// Timeout, so Kubernetes' ProgressDeadlineExceeded (which names the stuck
// ReplicaSet) normally fires first and the reconciler's own timer is the
// backstop for rollouts the controller cannot report on.
func (r *Reconciler) DeadlineSeconds() int32 {
	r.init()
	s := int32(r.Timeout * 9 / 10 / time.Second)
	if s < 1 {
		s = 1
	}
	return s
}

// ReconcileOnce runs one pass over in-flight and live deployments.
func (r *Reconciler) ReconcileOnce(ctx context.Context) error {
	r.init()
	inflight, err := r.Store.ListInFlight(ctx)
	if err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	for _, d := range inflight {
		if err := r.reconcileDeployment(ctx, d); err != nil {
			// One bad deployment must not stall the rest.
			r.Log.Warn("reconcile deployment failed", "deployment", d.ID, "model", d.ModelName, "env", d.Environment, "err", err)
		}
	}
	live, err := r.Store.ListLive(ctx)
	if err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	for _, d := range live {
		if err := r.checkHealth(ctx, d); err != nil {
			r.Log.Warn("health check failed", "deployment", d.ID, "err", err)
		}
	}
	return nil
}

func (r *Reconciler) age(d domain.Deployment) time.Duration {
	start := d.CreatedAt
	if d.StartedAt != nil {
		start = *d.StartedAt
	}
	return r.Now().Sub(start)
}

func (r *Reconciler) reconcileDeployment(ctx context.Context, d domain.Deployment) error {
	switch d.Status {
	case domain.DeployPending:
		v, err := r.Store.GetVersionByID(ctx, d.VersionID)
		if err != nil {
			return fmt.Errorf("load version for %s: %w", d.ID, err)
		}
		if err := r.Kube.Apply(ctx, SpecFor(d, v, r.DeadlineSeconds())); err != nil {
			if r.age(d) > r.Timeout {
				return r.Transitions.MarkFailed(ctx, d.ID, fmt.Sprintf("apply failed for %s: %v", r.Timeout, err))
			}
			return fmt.Errorf("apply %s (will retry): %w", d.ID, err)
		}
		if err := r.Transitions.MarkProgressing(ctx, d.ID); err != nil {
			return fmt.Errorf("mark %s progressing: %w", d.ID, err)
		}
		// Check immediately: a no-op re-apply may already be ready.
		d.Status = domain.DeployProgressing
		now := r.Now()
		d.StartedAt = &now
		return r.reconcileDeployment(ctx, d)

	case domain.DeployProgressing:
		st, err := r.Kube.Status(ctx, d.Namespace, d.ModelName)
		if err != nil {
			return fmt.Errorf("status of %s: %w", d.ID, err)
		}
		switch {
		case st.RolloutComplete(d.ID):
			return r.Transitions.MarkReady(ctx, d.ID)
		case st.Failed && st.DeploymentID == d.ID:
			return r.Transitions.MarkFailed(ctx, d.ID, st.Message)
		case r.age(d) > r.Timeout:
			return r.Transitions.MarkFailed(ctx, d.ID, fmt.Sprintf(
				"not ready within %s (updated=%d available=%d desired=%d)", r.Timeout, st.Updated, st.Available, st.Desired))
		}
	}
	return nil
}

func (r *Reconciler) checkHealth(ctx context.Context, d domain.Deployment) error {
	st, err := r.Kube.Status(ctx, d.Namespace, d.ModelName)
	if err != nil {
		return fmt.Errorf("status of %s: %w", d.ID, err)
	}
	h := st.Health()
	r.mu.Lock()
	prev, seen := r.lastHealth[d.ID]
	r.lastHealth[d.ID] = h
	r.mu.Unlock()
	// A freshly-ready deployment is healthy by definition; only report
	// changes after that so the topic carries signal, not heartbeat noise.
	if !seen {
		if h == "healthy" {
			return nil
		}
		prev = "healthy"
	}
	if prev == h {
		return nil
	}
	return r.Transitions.ReportHealth(ctx, d, h, st)
}

// Run reconciles every Interval (or when kicked) until ctx is cancelled,
// while holding the reconciler leader lock.
func (r *Reconciler) Run(ctx context.Context) {
	r.init()
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	for {
		if release == nil {
			rel, ok, err := r.Store.TryLeader(ctx, ReconcilerLockKey)
			if err != nil && ctx.Err() == nil {
				r.Log.Warn("reconciler: leader election failed", "err", err)
			}
			if ok {
				release = rel
				r.Log.Info("reconciler: acquired leadership")
			}
		}
		if release != nil {
			if err := r.ReconcileOnce(ctx); err != nil && ctx.Err() == nil {
				r.Log.Warn("reconcile pass failed", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.kick:
		}
	}
}
