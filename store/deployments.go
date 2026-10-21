package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/wenxuewu0216/modeldock/domain"
)

const deploymentCols = `d.id, d.model_id, m.name, d.version_id, v.version, d.environment, e.namespace, d.revision,
	d.kind, d.status, d.reason, d.created_by, d.created_at, d.updated_at, d.started_at, d.ready_at, d.finished_at`

const deploymentFrom = ` FROM deployments d
	JOIN models m ON m.id = d.model_id
	JOIN model_versions v ON v.id = d.version_id
	JOIN environments e ON e.name = d.environment `

func scanDeployment(r pgx.Row) (domain.Deployment, error) {
	var d domain.Deployment
	err := r.Scan(&d.ID, &d.ModelID, &d.ModelName, &d.VersionID, &d.Version, &d.Environment, &d.Namespace,
		&d.Revision, &d.Kind, &d.Status, &d.Reason, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt,
		&d.StartedAt, &d.ReadyAt, &d.FinishedAt)
	return d, err
}

func collectDeployments(rows pgx.Rows, err error, op string) ([]domain.Deployment, error) {
	if err != nil {
		return nil, wrap(op, err)
	}
	ds, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Deployment, error) { return scanDeployment(r) })
	return ds, wrap(op, err)
}

// CreateDeployment inserts a new revision for (model, environment). The
// revision number is the previous maximum plus one; callers must hold the
// model lock (Tx.LockModel) so revisions are gap-free and unique.
func (t *Tx) CreateDeployment(ctx context.Context, d *domain.Deployment) error {
	d.ID = uuid.NewString()
	err := t.db.QueryRow(ctx, `
		INSERT INTO deployments (id, model_id, version_id, environment, revision, kind, status, reason, created_by)
		SELECT $1, $2, $3, $4, COALESCE(MAX(revision), 0) + 1, $5, $6, $7, $8
		FROM deployments WHERE model_id = $2 AND environment = $4
		RETURNING revision, created_at, updated_at`,
		d.ID, d.ModelID, d.VersionID, d.Environment, d.Kind, d.Status, d.Reason, d.CreatedBy,
	).Scan(&d.Revision, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return wrap("create deployment", err)
	}
	return wrap("read namespace", t.db.QueryRow(ctx,
		`SELECT namespace FROM environments WHERE name = $1`, d.Environment).Scan(&d.Namespace))
}

// GetDeployment returns a deployment revision by ID.
func (q queries) GetDeployment(ctx context.Context, id string) (domain.Deployment, error) {
	if _, err := uuid.Parse(id); err != nil {
		return domain.Deployment{}, fmt.Errorf("store: get deployment %q: %w", id, domain.ErrNotFound)
	}
	d, err := scanDeployment(q.db.QueryRow(ctx, `SELECT `+deploymentCols+deploymentFrom+`WHERE d.id = $1`, id))
	return d, wrap("get deployment "+id, err)
}

// ListDeployments returns the deployment history of a model, newest first.
// env may be empty to include all environments.
func (q queries) ListDeployments(ctx context.Context, model string, env domain.Environment, limit int) ([]domain.Deployment, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.db.Query(ctx, `SELECT `+deploymentCols+deploymentFrom+`
		WHERE m.name = $1 AND ($2 = '' OR d.environment = $2)
		ORDER BY d.created_at DESC, d.revision DESC LIMIT $3`, model, string(env), limit)
	return collectDeployments(rows, err, "list deployments of "+model)
}

// CurrentDeployment returns the latest revision of model in env.
func (q queries) CurrentDeployment(ctx context.Context, modelID string, env domain.Environment) (domain.Deployment, error) {
	d, err := scanDeployment(q.db.QueryRow(ctx, `SELECT `+deploymentCols+deploymentFrom+`
		WHERE d.model_id = $1 AND d.environment = $2 ORDER BY d.revision DESC LIMIT 1`, modelID, env))
	return d, wrap(fmt.Sprintf("current deployment of %s in %s", modelID, env), err)
}

// WasReadyIn reports whether versionID ever had a ready deployment in env.
// Rollback targets must satisfy this: only known-good versions can be
// rolled back to.
func (q queries) WasReadyIn(ctx context.Context, versionID string, env domain.Environment) (bool, error) {
	var ok bool
	err := q.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deployments
		WHERE version_id = $1 AND environment = $2 AND ready_at IS NOT NULL)`, versionID, env).Scan(&ok)
	return ok, wrap("check version readiness history", err)
}

// PreviousGoodVersion returns the most recent version, other than
// excludeVersionID, that was ready in env. It is the default rollback target.
func (q queries) PreviousGoodVersion(ctx context.Context, modelID string, env domain.Environment, excludeVersionID string) (string, error) {
	var id string
	err := q.db.QueryRow(ctx, `SELECT version_id FROM deployments
		WHERE model_id = $1 AND environment = $2 AND ready_at IS NOT NULL AND version_id <> $3
		ORDER BY revision DESC LIMIT 1`, modelID, env, excludeVersionID).Scan(&id)
	return id, wrap("find previous good version", err)
}

// ListInFlight returns deployments the reconciler still owns, oldest first.
func (q queries) ListInFlight(ctx context.Context) ([]domain.Deployment, error) {
	rows, err := q.db.Query(ctx, `SELECT `+deploymentCols+deploymentFrom+`
		WHERE d.status IN ('pending','progressing') ORDER BY d.created_at`)
	return collectDeployments(rows, err, "list in-flight deployments")
}

// ListLive returns the latest ready revision for every (model, environment)
// whose current revision is ready. The reconciler health-checks these.
func (q queries) ListLive(ctx context.Context) ([]domain.Deployment, error) {
	rows, err := q.db.Query(ctx, `SELECT `+deploymentCols+deploymentFrom+`
		WHERE d.status = 'ready' AND d.revision = (SELECT MAX(revision) FROM deployments x
			WHERE x.model_id = d.model_id AND x.environment = d.environment)
		ORDER BY m.name, d.environment`)
	return collectDeployments(rows, err, "list live deployments")
}

// DeploymentUpdate describes a status change. Zero timestamps are left
// untouched.
type DeploymentUpdate struct {
	From       []domain.DeploymentStatus
	To         domain.DeploymentStatus
	Reason     string
	StartedAt  time.Time
	ReadyAt    time.Time
	FinishedAt time.Time
}

// UpdateDeployment applies u to deployment id as a compare-and-set on its
// status: if the stored status is not one of u.From, ErrConflict is
// returned. This makes reconciler/API races safe.
func (t *Tx) UpdateDeployment(ctx context.Context, id string, u DeploymentUpdate) (domain.Deployment, error) {
	from := make([]string, len(u.From))
	for i, s := range u.From {
		from[i] = string(s)
	}
	nt := func(ts time.Time) *time.Time {
		if ts.IsZero() {
			return nil
		}
		return &ts
	}
	tag, err := t.db.Exec(ctx, `UPDATE deployments SET status = $3,
			reason = CASE WHEN $4 = '' THEN reason ELSE $4 END,
			started_at = COALESCE($5, started_at),
			ready_at = COALESCE($6, ready_at),
			finished_at = COALESCE($7, finished_at),
			updated_at = now()
		WHERE id = $1 AND status = ANY($2)`,
		id, from, u.To, u.Reason, nt(u.StartedAt), nt(u.ReadyAt), nt(u.FinishedAt))
	if err != nil {
		return domain.Deployment{}, wrap("update deployment "+id, err)
	}
	if tag.RowsAffected() != 1 {
		return domain.Deployment{}, fmt.Errorf("store: update deployment %s to %s: %w: status is not one of %v",
			id, u.To, domain.ErrConflict, u.From)
	}
	return t.GetDeployment(ctx, id)
}
