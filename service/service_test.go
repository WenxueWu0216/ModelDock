package service_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wenxuewu0216/modeldock/domain"
	"github.com/wenxuewu0216/modeldock/events"
	"github.com/wenxuewu0216/modeldock/kube"
	"github.com/wenxuewu0216/modeldock/service"
	"github.com/wenxuewu0216/modeldock/store"
	"github.com/wenxuewu0216/modeldock/store/storetest"
)

var (
	dana  = domain.User{ID: "dana", Role: domain.RoleDeployer}
	derek = domain.User{ID: "derek", Role: domain.RoleDeployer}
	alice = domain.User{ID: "alice", Role: domain.RoleApprover}
	vic   = domain.User{ID: "vic", Role: domain.RoleViewer}
	root  = domain.User{ID: "root", Role: domain.RoleAdmin}
)

type env struct {
	t     *testing.T
	ctx   context.Context
	store *store.Store
	svc   *service.Service
	kube  *kube.Fake
	rec   *kube.Reconciler
}

func newEnv(t *testing.T, autoRollback bool) *env {
	t.Helper()
	s := storetest.New(t)
	fake := kube.NewFake()
	svc := service.New(s, nil, nil, autoRollback)
	rec := &kube.Reconciler{Store: s, Kube: fake, Transitions: svc, Timeout: time.Minute}
	svc.Kicker = rec
	return &env{t: t, ctx: context.Background(), store: s, svc: svc, kube: fake, rec: rec}
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

// reconcile runs reconciler passes until nothing is in flight.
func (e *env) reconcile() {
	e.t.Helper()
	for i := 0; i < 10; i++ {
		e.must(e.rec.ReconcileOnce(e.ctx))
		inflight, err := e.store.ListInFlight(e.ctx)
		e.must(err)
		if len(inflight) == 0 {
			return
		}
	}
	e.t.Fatal("deployments still in flight after 10 passes")
}

func (e *env) register(model, version string) {
	e.t.Helper()
	if _, err := e.svc.GetModel(e.ctx, vic, model); errors.Is(err, domain.ErrNotFound) {
		_, err := e.svc.CreateModel(e.ctx, dana, model, "test model")
		e.must(err)
	}
	_, err := e.svc.RegisterVersion(e.ctx, dana, model, domain.ModelVersion{
		Version: version, Image: "ghcr.io/acme/" + model + ":" + version,
		Metadata: map[string]string{"framework": "sklearn"},
	})
	e.must(err)
}

func (e *env) state(model, version string) domain.VersionState {
	e.t.Helper()
	v, err := e.store.GetVersion(e.ctx, model, version)
	e.must(err)
	return v.State
}

// toProd drives model/version through staging and approval into prod.
func (e *env) toProd(model, version string) domain.Deployment {
	e.t.Helper()
	_, err := e.svc.DeployToStaging(e.ctx, dana, model, version)
	e.must(err)
	e.reconcile()
	a, err := e.svc.RequestPromotion(e.ctx, dana, model, version, "ship it")
	e.must(err)
	_, err = e.svc.Decide(e.ctx, alice, a.ID, true, "lgtm")
	e.must(err)
	d, err := e.svc.Promote(e.ctx, dana, model, version)
	e.must(err)
	e.reconcile()
	return d
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("err = %v, want %v", err, target)
	}
}

func TestFullLifecycle(t *testing.T) {
	e := newEnv(t, false)
	e.register("iris", "1.0.0")
	if e.state("iris", "1.0.0") != domain.StateRegistered {
		t.Fatal("not registered")
	}

	d, err := e.svc.DeployToStaging(e.ctx, dana, "iris", "1.0.0")
	e.must(err)
	if d.Status != domain.DeployPending || d.Namespace != "modeldock-staging" || d.Revision != 1 {
		t.Fatalf("deployment = %+v", d)
	}
	// Promotion cannot be requested before staging is ready.
	_, err = e.svc.RequestPromotion(e.ctx, dana, "iris", "1.0.0", "")
	wantErr(t, err, domain.ErrConflict)

	e.reconcile()
	got, _ := e.svc.GetDeployment(e.ctx, vic, d.ID)
	if got.Status != domain.DeployReady || got.ReadyAt == nil {
		t.Fatalf("staging deployment = %+v", got)
	}
	spec, objs, ok := e.kube.Applied("modeldock-staging", "iris")
	if !ok || spec.Image != "ghcr.io/acme/iris:1.0.0" || objs.HPA == nil || objs.Service == nil {
		t.Fatal("Deployment+Service+HPA not applied to staging")
	}

	// Promote without approval is refused.
	_, err = e.svc.Promote(e.ctx, dana, "iris", "1.0.0")
	wantErr(t, err, domain.ErrConflict)

	a, err := e.svc.RequestPromotion(e.ctx, dana, "iris", "1.0.0", "accuracy +2%")
	e.must(err)
	if e.state("iris", "1.0.0") != domain.StatePendingApproval {
		t.Fatal("not pending approval")
	}
	_, err = e.svc.Promote(e.ctx, dana, "iris", "1.0.0")
	wantErr(t, err, domain.ErrConflict)

	_, err = e.svc.Decide(e.ctx, alice, a.ID, true, "lgtm")
	e.must(err)
	if e.state("iris", "1.0.0") != domain.StateApproved {
		t.Fatal("not approved")
	}
	pd, err := e.svc.Promote(e.ctx, dana, "iris", "1.0.0")
	e.must(err)
	e.reconcile()
	if e.state("iris", "1.0.0") != domain.StateProduction {
		t.Fatal("not in production")
	}
	if _, _, ok := e.kube.Applied("modeldock-prod", "iris"); !ok {
		t.Fatal("not applied to prod namespace")
	}

	// v2 through the same path supersedes v1.
	e.register("iris", "1.1.0")
	e.toProd("iris", "1.1.0")
	if e.state("iris", "1.0.0") != domain.StateSuperseded || e.state("iris", "1.1.0") != domain.StateProduction {
		t.Fatalf("v1=%s v2=%s", e.state("iris", "1.0.0"), e.state("iris", "1.1.0"))
	}

	// One-call rollback to the previous good version.
	rb, err := e.svc.Rollback(e.ctx, dana, "iris", domain.EnvProd, "")
	e.must(err)
	if rb.Version != "1.0.0" || rb.Kind != domain.KindRollback || rb.Revision != 3 {
		t.Fatalf("rollback = %+v", rb)
	}
	e.reconcile()
	if e.state("iris", "1.0.0") != domain.StateProduction || e.state("iris", "1.1.0") != domain.StateRolledBack {
		t.Fatalf("after rollback v1=%s v2=%s", e.state("iris", "1.0.0"), e.state("iris", "1.1.0"))
	}
	spec, _, _ = e.kube.Applied("modeldock-prod", "iris")
	if spec.Image != "ghcr.io/acme/iris:1.0.0" {
		t.Fatalf("prod image after rollback = %s", spec.Image)
	}

	// History and status.
	hist, err := e.svc.History(e.ctx, vic, "iris", domain.EnvProd, 0)
	e.must(err)
	if len(hist) != 3 || hist[0].Status != domain.DeployReady || hist[1].Status != domain.DeployRolledBack ||
		hist[2].Status != domain.DeploySuperseded || hist[2].ID != pd.ID {
		t.Fatalf("prod history = %+v", hist)
	}
	st, err := e.svc.Status(e.ctx, vic, "iris")
	e.must(err)
	if st.Current[domain.EnvProd].Version != "1.0.0" || st.Current[domain.EnvStaging].Version != "1.1.0" {
		t.Fatalf("status = %+v", st)
	}

	// Diff.
	diff, err := e.svc.Diff(e.ctx, vic, "iris", "1.0.0", "1.1.0")
	e.must(err)
	if len(diff.Changes) != 1 || diff.Changes[0].Field != "image" {
		t.Fatalf("diff = %+v", diff)
	}

	// The audit log recorded every step, in order.
	evs, err := e.svc.Audit(e.ctx, vic, "iris", 0, 100)
	e.must(err)
	var types []string
	for i := len(evs) - 1; i >= 0; i-- {
		types = append(types, evs[i].Type)
	}
	want := []string{
		events.TypeModelRegistered, events.TypeVersionRegistered,
		events.TypeDeployRequested, events.TypeDeployReady,
		events.TypePromotionRequested, events.TypeApprovalApproved, events.TypePromoteStarted, events.TypeDeployReady,
		events.TypeVersionRegistered,
		events.TypeDeployRequested, events.TypeDeployReady,
		events.TypePromotionRequested, events.TypeApprovalApproved, events.TypePromoteStarted, events.TypeDeployReady,
		events.TypeRollbackStarted, events.TypeDeployReady,
	}
	if len(types) != len(want) {
		t.Fatalf("audit types =\n%v\nwant\n%v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("audit[%d] = %s, want %s (all: %v)", i, types[i], want[i], types)
		}
	}
	// Every audit event is also in the outbox, unpublished.
	n, _, err := e.store.OutboxBacklog(e.ctx)
	e.must(err)
	if n != len(want) {
		t.Fatalf("outbox backlog = %d, want %d", n, len(want))
	}
}

func TestRBACAndFourEyes(t *testing.T) {
	e := newEnv(t, false)
	e.register("iris", "1")
	_, err := e.svc.DeployToStaging(e.ctx, dana, "iris", "1")
	e.must(err)
	e.reconcile()

	// Viewers and approvers cannot deploy; viewers cannot register.
	_, err = e.svc.DeployToStaging(e.ctx, vic, "iris", "1")
	wantErr(t, err, domain.ErrForbidden)
	_, err = e.svc.DeployToStaging(e.ctx, alice, "iris", "1")
	wantErr(t, err, domain.ErrForbidden)
	_, err = e.svc.CreateModel(e.ctx, vic, "other", "")
	wantErr(t, err, domain.ErrForbidden)

	a, err := e.svc.RequestPromotion(e.ctx, dana, "iris", "1", "")
	e.must(err)

	// The requesting deployer cannot approve; neither can another deployer.
	_, err = e.svc.Decide(e.ctx, dana, a.ID, true, "")
	wantErr(t, err, domain.ErrForbidden)
	_, err = e.svc.Decide(e.ctx, derek, a.ID, true, "")
	wantErr(t, err, domain.ErrForbidden)
	// Approvers cannot promote.
	_, err = e.svc.Promote(e.ctx, alice, "iris", "1")
	wantErr(t, err, domain.ErrForbidden)

	// An admin who requests a promotion cannot approve it either.
	e.register("iris", "2")
	_, err = e.svc.DeployToStaging(e.ctx, root, "iris", "2")
	e.must(err)
	e.reconcile()
	_, err = e.svc.Decide(e.ctx, alice, a.ID, false, "needs more eval") // reject v1
	e.must(err)
	if e.state("iris", "1") != domain.StateStaging {
		t.Fatal("rejected version should return to staging")
	}
	a2, err := e.svc.RequestPromotion(e.ctx, root, "iris", "2", "")
	e.must(err)
	_, err = e.svc.Decide(e.ctx, root, a2.ID, true, "")
	wantErr(t, err, domain.ErrForbidden)

	// Decisions are final.
	_, err = e.svc.Decide(e.ctx, alice, a.ID, true, "")
	wantErr(t, err, domain.ErrConflict)

	// Approved by someone else -> promotable.
	_, err = e.svc.Decide(e.ctx, alice, a2.ID, true, "")
	e.must(err)
	_, err = e.svc.Promote(e.ctx, dana, "iris", "2")
	e.must(err)
}

func TestFailedRolloutWithAutoRollback(t *testing.T) {
	e := newEnv(t, true)
	e.register("iris", "1")
	e.register("iris", "2")
	e.toProd("iris", "1")

	// v2 passes staging but crash-loops in prod.
	_, err := e.svc.DeployToStaging(e.ctx, dana, "iris", "2")
	e.must(err)
	e.reconcile()
	a, err := e.svc.RequestPromotion(e.ctx, dana, "iris", "2", "")
	e.must(err)
	_, err = e.svc.Decide(e.ctx, alice, a.ID, true, "")
	e.must(err)
	e.kube.OutcomeFor = func(s kube.WorkloadSpec) kube.Outcome {
		if s.Environment == domain.EnvProd && s.Version == "2" {
			return kube.FailFast
		}
		return kube.BecomeReady
	}
	_, err = e.svc.Promote(e.ctx, dana, "iris", "2")
	e.must(err)
	e.reconcile()

	if e.state("iris", "2") != domain.StateFailed || e.state("iris", "1") != domain.StateProduction {
		t.Fatalf("v1=%s v2=%s", e.state("iris", "1"), e.state("iris", "2"))
	}
	hist, err := e.svc.History(e.ctx, vic, "iris", domain.EnvProd, 0)
	e.must(err)
	if hist[0].Kind != domain.KindAutoRollback || hist[0].Version != "1" || hist[0].Status != domain.DeployReady {
		t.Fatalf("latest prod revision = %+v", hist[0])
	}
	if hist[1].Status != domain.DeployFailed || hist[1].Reason == "" {
		t.Fatalf("failed revision = %+v", hist[1])
	}
	spec, _, _ := e.kube.Applied("modeldock-prod", "iris")
	if spec.Version != "1" {
		t.Fatalf("prod running %s after auto-rollback", spec.Version)
	}
}

func TestFailedRolloutWithoutAutoRollback(t *testing.T) {
	e := newEnv(t, false)
	e.register("iris", "1")
	e.kube.OutcomeFor = func(kube.WorkloadSpec) kube.Outcome { return kube.NeverReady }
	now := time.Now()
	e.rec.Now = func() time.Time { return now }
	_, err := e.svc.DeployToStaging(e.ctx, dana, "iris", "1")
	e.must(err)
	e.must(e.rec.ReconcileOnce(e.ctx))
	now = now.Add(2 * time.Minute) // past the 1m timeout
	e.must(e.rec.ReconcileOnce(e.ctx))
	if e.state("iris", "1") != domain.StateFailed {
		t.Fatalf("state = %s", e.state("iris", "1"))
	}
	// Nothing was ever ready, so there is nothing to roll back to.
	_, err = e.svc.Rollback(e.ctx, dana, "iris", domain.EnvStaging, "")
	wantErr(t, err, domain.ErrConflict)
	// A failed version can be redeployed after a fix to the cluster.
	e.kube.OutcomeFor = nil
	_, err = e.svc.DeployToStaging(e.ctx, dana, "iris", "1")
	e.must(err)
	e.reconcile()
	if e.state("iris", "1") != domain.StateStaging {
		t.Fatalf("state = %s", e.state("iris", "1"))
	}
}

func TestRollbackGuards(t *testing.T) {
	e := newEnv(t, false)
	e.register("iris", "1")
	e.register("iris", "2")
	_, err := e.svc.Rollback(e.ctx, dana, "iris", domain.EnvProd, "")
	wantErr(t, err, domain.ErrConflict) // never deployed
	e.toProd("iris", "1")
	_, err = e.svc.Rollback(e.ctx, dana, "iris", domain.EnvProd, "1")
	wantErr(t, err, domain.ErrConflict) // already current
	_, err = e.svc.Rollback(e.ctx, dana, "iris", domain.EnvProd, "2")
	wantErr(t, err, domain.ErrConflict) // never ready in prod: rollback cannot bypass approval
	_, err = e.svc.Rollback(e.ctx, vic, "iris", domain.EnvProd, "")
	wantErr(t, err, domain.ErrForbidden)

	// Staging rollback to an explicit version.
	_, err = e.svc.DeployToStaging(e.ctx, dana, "iris", "2")
	e.must(err)
	e.reconcile()
	d, err := e.svc.Rollback(e.ctx, dana, "iris", domain.EnvStaging, "1")
	e.must(err)
	e.reconcile()
	if d.Version != "1" || e.state("iris", "2") != domain.StateRolledBack {
		t.Fatalf("staging rollback: %+v, v2=%s", d, e.state("iris", "2"))
	}
	// v1 is live in prod; a staging rollback must not change that.
	if e.state("iris", "1") != domain.StateProduction {
		t.Fatalf("v1 = %s", e.state("iris", "1"))
	}
}

func TestConcurrentApproversOnlyOneWins(t *testing.T) {
	e := newEnv(t, false)
	e.register("iris", "1")
	_, err := e.svc.DeployToStaging(e.ctx, dana, "iris", "1")
	e.must(err)
	e.reconcile()
	a, err := e.svc.RequestPromotion(e.ctx, dana, "iris", "1", "")
	e.must(err)

	approvers := []domain.User{alice, {ID: "bob", Role: domain.RoleApprover}, {ID: "carol", Role: domain.RoleApprover},
		{ID: "root2", Role: domain.RoleAdmin}}
	var wg sync.WaitGroup
	errs := make([]error, len(approvers))
	for i, u := range approvers {
		wg.Add(1)
		go func(i int, u domain.User) {
			defer wg.Done()
			_, errs[i] = e.svc.Decide(e.ctx, u, a.ID, i%2 == 0, "")
		}(i, u)
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else if !errors.Is(err, domain.ErrConflict) {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d approvers won, want exactly 1 (%v)", wins, errs)
	}
	evs, err := e.svc.Audit(e.ctx, vic, "iris", 0, 100)
	e.must(err)
	decisions := 0
	for _, ev := range evs {
		if ev.Type == events.TypeApprovalApproved || ev.Type == events.TypeApprovalRejected {
			decisions++
		}
	}
	if decisions != 1 {
		t.Fatalf("%d decision events, want 1", decisions)
	}
}

func TestConcurrentDeploysGetDistinctRevisions(t *testing.T) {
	e := newEnv(t, false)
	e.register("iris", "1")
	var wg sync.WaitGroup
	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.svc.DeployToStaging(e.ctx, dana, "iris", "1")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		e.must(err)
	}
	hist, err := e.svc.History(e.ctx, vic, "iris", domain.EnvStaging, 0)
	e.must(err)
	seen := map[int]bool{}
	inflight := 0
	for _, d := range hist {
		if seen[d.Revision] {
			t.Fatalf("duplicate revision %d", d.Revision)
		}
		seen[d.Revision] = true
		if d.Status.InFlight() {
			inflight++
		}
	}
	if len(hist) != n || inflight != 1 {
		t.Fatalf("revisions=%d inflight=%d; older revisions must be superseded", len(hist), inflight)
	}
}

func TestHealthEventsReachAuditLog(t *testing.T) {
	e := newEnv(t, false)
	e.register("iris", "1")
	_, err := e.svc.RegisterVersion(e.ctx, dana, "iris", domain.ModelVersion{Version: "2", Image: "x:2",
		Scaling: domain.Scaling{MinReplicas: 2, MaxReplicas: 4}})
	e.must(err)
	_, err = e.svc.DeployToStaging(e.ctx, dana, "iris", "2")
	e.must(err)
	e.reconcile()
	e.must(e.rec.ReconcileOnce(e.ctx))
	e.kube.SetDegraded("modeldock-staging", "iris", true)
	e.must(e.rec.ReconcileOnce(e.ctx))
	evs, err := e.svc.Audit(e.ctx, vic, "iris", 0, 1)
	e.must(err)
	if len(evs) != 1 || evs[0].Type != events.TypeInferenceHealth || evs[0].Topic() != events.TopicInferenceHealth {
		t.Fatalf("latest event = %+v", evs)
	}
}

func TestValidation(t *testing.T) {
	e := newEnv(t, false)
	_, err := e.svc.CreateModel(e.ctx, dana, "Bad_Name", "")
	wantErr(t, err, domain.ErrInvalid)
	_, err = e.svc.CreateModel(e.ctx, dana, "iris", "")
	e.must(err)
	_, err = e.svc.CreateModel(e.ctx, dana, "iris", "")
	wantErr(t, err, domain.ErrConflict)
	_, err = e.svc.RegisterVersion(e.ctx, dana, "iris", domain.ModelVersion{Version: "1", Image: "x:1",
		Resources: domain.Resources{CPURequest: "a lot"}})
	wantErr(t, err, domain.ErrInvalid)
	_, err = e.svc.RegisterVersion(e.ctx, dana, "nope", domain.ModelVersion{Version: "1", Image: "x:1"})
	wantErr(t, err, domain.ErrNotFound)
	_, err = e.svc.DeployToStaging(e.ctx, dana, "iris", "404")
	wantErr(t, err, domain.ErrNotFound)
}
