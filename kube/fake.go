package kube

import (
	"context"
	"fmt"
	"sync"
)

// Outcome scripts how a fake workload behaves after Apply.
type Outcome int

const (
	// BecomeReady: the rollout completes after ReadyAfterPolls Status calls.
	BecomeReady Outcome = iota
	// NeverReady: replicas never become available (e.g. crash loop); the
	// reconciler's timeout must fire.
	NeverReady
	// FailFast: Kubernetes reports ProgressDeadlineExceeded immediately.
	FailFast
)

type fakeWorkload struct {
	spec       WorkloadSpec
	objs       Objects
	generation int64
	polls      int
	outcome    Outcome
	// degraded forces Available below Desired once ready (health tests).
	degraded bool
}

// Fake is an in-memory Client. It renders real objects through Build so
// manifest bugs surface in tests too, then simulates rollout progress.
// Safe for concurrent use.
type Fake struct {
	mu        sync.Mutex
	workloads map[string]*fakeWorkload
	// ReadyAfterPolls is how many Status calls a BecomeReady rollout needs.
	ReadyAfterPolls int
	// OutcomeFor picks the outcome for each Apply; default BecomeReady.
	OutcomeFor func(WorkloadSpec) Outcome
	// ApplyErr, if set, is returned by Apply (API server unavailable).
	ApplyErr error
	applies  int
}

// NewFake returns a Fake whose rollouts become ready after one poll.
func NewFake() *Fake {
	return &Fake{workloads: map[string]*fakeWorkload{}, ReadyAfterPolls: 1}
}

func key(ns, name string) string { return ns + "/" + name }

// Apply implements Client.
func (f *Fake) Apply(_ context.Context, spec WorkloadSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ApplyErr != nil {
		return fmt.Errorf("kube fake: apply %s/%s: %w", spec.Namespace, spec.Name, f.ApplyErr)
	}
	objs, err := Build(spec, 0)
	if err != nil {
		return fmt.Errorf("kube fake: build %s/%s: %w", spec.Namespace, spec.Name, err)
	}
	f.applies++
	w := f.workloads[key(spec.Namespace, spec.Name)]
	if w == nil {
		w = &fakeWorkload{}
		f.workloads[key(spec.Namespace, spec.Name)] = w
	}
	if w.spec.DeploymentID != spec.DeploymentID {
		// New template: generation bumps and progress restarts. Re-applying
		// the same deployment (reconciler retry) is a no-op, like SSA.
		w.generation++
		w.polls = 0
		w.degraded = false
	}
	w.spec, w.objs = spec, objs
	w.outcome = BecomeReady
	if f.OutcomeFor != nil {
		w.outcome = f.OutcomeFor(spec)
	}
	return nil
}

// Status implements Client.
func (f *Fake) Status(_ context.Context, ns, name string) (WorkloadStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.workloads[key(ns, name)]
	if w == nil {
		return WorkloadStatus{}, nil
	}
	w.polls++
	desired := *w.objs.Deployment.Spec.Replicas
	st := WorkloadStatus{
		Exists: true, DeploymentID: w.spec.DeploymentID,
		Generation: w.generation, Observed: w.generation, Desired: desired, Total: desired,
	}
	switch w.outcome {
	case BecomeReady:
		if w.polls >= f.ReadyAfterPolls {
			st.Updated, st.Ready, st.Available = desired, desired, desired
			if w.degraded {
				st.Ready, st.Available = desired-1, desired-1
			}
		}
	case NeverReady:
		st.Updated = desired
	case FailFast:
		st.Failed = true
		st.Message = "ProgressDeadlineExceeded: ReplicaSet has timed out progressing"
	}
	return st, nil
}

// Delete implements Client.
func (f *Fake) Delete(_ context.Context, ns, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.workloads, key(ns, name))
	return nil
}

// Applied returns the last applied spec for ns/name.
func (f *Fake) Applied(ns, name string) (WorkloadSpec, Objects, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.workloads[key(ns, name)]
	if w == nil {
		return WorkloadSpec{}, Objects{}, false
	}
	return w.spec, w.objs, true
}

// SetDegraded makes a ready workload report one unavailable replica.
func (f *Fake) SetDegraded(ns, name string, degraded bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if w := f.workloads[key(ns, name)]; w != nil {
		w.degraded = degraded
	}
}

// ApplyCount returns how many successful Apply calls were made.
func (f *Fake) ApplyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applies
}
