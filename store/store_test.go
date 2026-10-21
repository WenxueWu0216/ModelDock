package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wenxuewu0216/modeldock/domain"
	"github.com/wenxuewu0216/modeldock/events"
	"github.com/wenxuewu0216/modeldock/store"
	"github.com/wenxuewu0216/modeldock/store/storetest"
)

func seed(t *testing.T, s *store.Store) (domain.Model, domain.ModelVersion) {
	t.Helper()
	ctx := context.Background()
	m := domain.Model{Name: "iris", CreatedBy: "dana"}
	v := domain.ModelVersion{Version: "1.0.0", Image: "ghcr.io/acme/iris:1.0.0", CreatedBy: "dana",
		Metadata: map[string]string{"framework": "sklearn"}}
	if err := v.Validate(); err != nil {
		t.Fatal(err)
	}
	err := s.InTx(ctx, func(tx *store.Tx) error {
		if err := tx.CreateModel(ctx, &m); err != nil {
			return err
		}
		return tx.CreateVersion(ctx, m, &v)
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, v
}

func TestRegistryRoundTrip(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	m, v := seed(t, s)

	got, err := s.GetVersion(ctx, "iris", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != v.ID || got.Metadata["framework"] != "sklearn" || got.Scaling.MaxReplicas != 3 || got.ModelName != "iris" {
		t.Fatalf("got %+v", got)
	}
	if _, err := s.GetVersion(ctx, "iris", "9"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing version err = %v", err)
	}
	dup := domain.Model{Name: m.Name, CreatedBy: "x"}
	err = s.InTx(ctx, func(tx *store.Tx) error { return tx.CreateModel(ctx, &dup) })
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate model err = %v", err)
	}
}

func TestVersionStateCompareAndSet(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	_, v := seed(t, s)
	err := s.InTx(ctx, func(tx *store.Tx) error {
		return tx.SetVersionState(ctx, v.ID, domain.StateRegistered, domain.StateStaging)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Stale "from" loses.
	err = s.InTx(ctx, func(tx *store.Tx) error {
		return tx.SetVersionState(ctx, v.ID, domain.StateRegistered, domain.StateStaging)
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale CAS err = %v", err)
	}
}

func TestDeploymentRevisionsAndHistory(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	m, v := seed(t, s)
	var ids []string
	for i := 0; i < 3; i++ {
		d := domain.Deployment{ModelID: m.ID, VersionID: v.ID, Environment: domain.EnvStaging,
			Kind: domain.KindDeploy, Status: domain.DeployPending, CreatedBy: "dana"}
		if err := s.InTx(ctx, func(tx *store.Tx) error { return tx.CreateDeployment(ctx, &d) }); err != nil {
			t.Fatal(err)
		}
		if d.Revision != i+1 || d.Namespace != "modeldock-staging" {
			t.Fatalf("revision=%d ns=%s", d.Revision, d.Namespace)
		}
		ids = append(ids, d.ID)
	}
	cur, err := s.CurrentDeployment(ctx, m.ID, domain.EnvStaging)
	if err != nil || cur.ID != ids[2] {
		t.Fatalf("current = %+v, %v", cur, err)
	}
	if _, err := s.CurrentDeployment(ctx, m.ID, domain.EnvProd); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("prod current err = %v", err)
	}
	inflight, err := s.ListInFlight(ctx)
	if err != nil || len(inflight) != 3 {
		t.Fatalf("in flight = %d, %v", len(inflight), err)
	}
	// CAS on deployment status.
	err = s.InTx(ctx, func(tx *store.Tx) error {
		_, err := tx.UpdateDeployment(ctx, ids[0], store.DeploymentUpdate{
			From: []domain.DeploymentStatus{domain.DeployProgressing}, To: domain.DeployReady})
		return err
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("CAS from wrong status err = %v", err)
	}
	hist, err := s.ListDeployments(ctx, "iris", "", 10)
	if err != nil || len(hist) != 3 || hist[0].Revision != 3 {
		t.Fatalf("history = %+v, %v", hist, err)
	}
}

func TestSelfApprovalBlockedByConstraint(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	_, v := seed(t, s)
	a := domain.Approval{VersionID: v.ID, RequestedBy: "dana"}
	if err := s.InTx(ctx, func(tx *store.Tx) error { return tx.CreateApproval(ctx, &a) }); err != nil {
		t.Fatal(err)
	}
	// Second pending request for the same version violates the partial index.
	b := domain.Approval{VersionID: v.ID, RequestedBy: "eve"}
	if err := s.InTx(ctx, func(tx *store.Tx) error { return tx.CreateApproval(ctx, &b) }); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate pending err = %v", err)
	}
	// Even if the application check were bypassed, the DB refuses self-approval.
	err := s.InTx(ctx, func(tx *store.Tx) error {
		_, err := tx.DecideApproval(ctx, a.ID, domain.ApprovalApproved, "dana", "")
		return err
	})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("self approval err = %v", err)
	}
	err = s.InTx(ctx, func(tx *store.Tx) error {
		_, err := tx.DecideApproval(ctx, a.ID, domain.ApprovalApproved, "alice", "lgtm")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAuditLogIsAppendOnly(t *testing.T) {
	url := storetest.URL(t)
	s, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	ev := events.New(events.TypeModelRegistered, "dana", map[string]string{"k": "v"})
	ev.Model = "iris"
	if err := s.InTx(ctx, func(tx *store.Tx) error { return tx.AppendEvent(ctx, &ev) }); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`UPDATE audit_events SET actor = 'mallory'`,
		`DELETE FROM audit_events`,
		`TRUNCATE audit_events CASCADE`,
	} {
		_, err := conn.Exec(ctx, q)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err = %v, want append-only rejection", q, err)
		}
	}
	got, err := s.ListAudit(ctx, "iris", 0, 10)
	if err != nil || len(got) != 1 || got[0].Actor != "dana" {
		t.Fatalf("audit = %+v, %v", got, err)
	}
}

func TestRollbackOfTxDropsEvent(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	boom := errors.New("boom")
	err := s.InTx(ctx, func(tx *store.Tx) error {
		ev := events.New(events.TypeDeployRequested, "dana", nil)
		if err := tx.AppendEvent(ctx, &ev); err != nil {
			return err
		}
		return boom // state change failed after the event was written
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	n, _, err := s.OutboxBacklog(ctx)
	if err != nil || n != 0 {
		t.Fatalf("backlog = %d, %v; event must not outlive its rolled-back transaction", n, err)
	}
	evs, _ := s.ListAudit(ctx, "", 0, 10)
	if len(evs) != 0 {
		t.Fatalf("audit has %d events", len(evs))
	}
}

func TestDashboardDedup(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	ev := events.New(events.TypeDeployFailed, "reconciler", nil)
	ev.Model, ev.Version, ev.Environment = "iris", "1", "staging"
	for i := 0; i < 3; i++ {
		applied, err := s.ApplyDashboardEvent(ctx, ev)
		if err != nil {
			t.Fatal(err)
		}
		if applied != (i == 0) {
			t.Fatalf("delivery %d applied=%v", i, applied)
		}
	}
	rows, err := s.ListDashboard(ctx)
	if err != nil || len(rows) != 1 || rows[0].DeploysFailed != 1 || rows[0].DeploysTotal != 1 {
		t.Fatalf("dashboard = %+v, %v", rows, err)
	}
}
