package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/wenxuewu0216/modeldock/domain"
)

// CreateModel inserts m, assigning its ID and creation time.
func (t *Tx) CreateModel(ctx context.Context, m *domain.Model) error {
	m.ID = uuid.NewString()
	err := t.db.QueryRow(ctx,
		`INSERT INTO models (id, name, description, created_by) VALUES ($1,$2,$3,$4) RETURNING created_at`,
		m.ID, m.Name, m.Description, m.CreatedBy).Scan(&m.CreatedAt)
	return wrap("create model "+m.Name, err)
}

const modelCols = `id, name, description, created_by, created_at`

func scanModel(r pgx.Row) (domain.Model, error) {
	var m domain.Model
	err := r.Scan(&m.ID, &m.Name, &m.Description, &m.CreatedBy, &m.CreatedAt)
	return m, err
}

// GetModel returns the model with the given name.
func (q queries) GetModel(ctx context.Context, name string) (domain.Model, error) {
	m, err := scanModel(q.db.QueryRow(ctx, `SELECT `+modelCols+` FROM models WHERE name = $1`, name))
	return m, wrap("get model "+name, err)
}

// LockModel returns the model and row-locks it until the transaction ends.
// Every operation that creates a deployment revision locks the model first,
// which serialises revision numbering and state transitions per model.
func (t *Tx) LockModel(ctx context.Context, name string) (domain.Model, error) {
	m, err := scanModel(t.db.QueryRow(ctx, `SELECT `+modelCols+` FROM models WHERE name = $1 FOR UPDATE`, name))
	return m, wrap("lock model "+name, err)
}

// ListModels returns all models ordered by name.
func (q queries) ListModels(ctx context.Context) ([]domain.Model, error) {
	rows, err := q.db.Query(ctx, `SELECT `+modelCols+` FROM models ORDER BY name`)
	if err != nil {
		return nil, wrap("list models", err)
	}
	ms, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Model, error) { return scanModel(r) })
	return ms, wrap("list models", err)
}

// CreateVersion inserts v (already validated) for model m.
func (t *Tx) CreateVersion(ctx context.Context, m domain.Model, v *domain.ModelVersion) error {
	v.ID = uuid.NewString()
	v.ModelID = m.ID
	v.ModelName = m.Name
	md, err := json.Marshal(v.Metadata)
	if err != nil {
		return fmt.Errorf("store: marshal metadata: %w", err)
	}
	res, err := json.Marshal(v.Resources)
	if err != nil {
		return fmt.Errorf("store: marshal resources: %w", err)
	}
	sc, err := json.Marshal(v.Scaling)
	if err != nil {
		return fmt.Errorf("store: marshal scaling: %w", err)
	}
	err = t.db.QueryRow(ctx, `
		INSERT INTO model_versions (id, model_id, version, image, port, metadata, resources, scaling, state, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING created_at, updated_at`,
		v.ID, v.ModelID, v.Version, v.Image, v.Port, md, res, sc, v.State, v.CreatedBy,
	).Scan(&v.CreatedAt, &v.UpdatedAt)
	return wrap(fmt.Sprintf("create version %s/%s", m.Name, v.Version), err)
}

const versionCols = `v.id, v.model_id, m.name, v.version, v.image, v.port, v.metadata, v.resources, v.scaling,
	v.state, v.created_by, v.created_at, v.updated_at`

func scanVersion(r pgx.Row) (domain.ModelVersion, error) {
	var v domain.ModelVersion
	var md, res, sc []byte
	if err := r.Scan(&v.ID, &v.ModelID, &v.ModelName, &v.Version, &v.Image, &v.Port, &md, &res, &sc,
		&v.State, &v.CreatedBy, &v.CreatedAt, &v.UpdatedAt); err != nil {
		return v, err
	}
	if err := json.Unmarshal(md, &v.Metadata); err != nil {
		return v, fmt.Errorf("decode metadata: %w", err)
	}
	if err := json.Unmarshal(res, &v.Resources); err != nil {
		return v, fmt.Errorf("decode resources: %w", err)
	}
	if err := json.Unmarshal(sc, &v.Scaling); err != nil {
		return v, fmt.Errorf("decode scaling: %w", err)
	}
	return v, nil
}

// GetVersion returns model/version.
func (q queries) GetVersion(ctx context.Context, model, version string) (domain.ModelVersion, error) {
	v, err := scanVersion(q.db.QueryRow(ctx, `SELECT `+versionCols+`
		FROM model_versions v JOIN models m ON m.id = v.model_id
		WHERE m.name = $1 AND v.version = $2`, model, version))
	return v, wrap(fmt.Sprintf("get version %s/%s", model, version), err)
}

// GetVersionByID returns a version by its ID.
func (q queries) GetVersionByID(ctx context.Context, id string) (domain.ModelVersion, error) {
	v, err := scanVersion(q.db.QueryRow(ctx, `SELECT `+versionCols+`
		FROM model_versions v JOIN models m ON m.id = v.model_id WHERE v.id = $1`, id))
	return v, wrap("get version "+id, err)
}

// ListVersions returns a model's versions, newest first.
func (q queries) ListVersions(ctx context.Context, model string) ([]domain.ModelVersion, error) {
	rows, err := q.db.Query(ctx, `SELECT `+versionCols+`
		FROM model_versions v JOIN models m ON m.id = v.model_id
		WHERE m.name = $1 ORDER BY v.created_at DESC, v.version DESC`, model)
	if err != nil {
		return nil, wrap("list versions of "+model, err)
	}
	vs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.ModelVersion, error) { return scanVersion(r) })
	return vs, wrap("list versions of "+model, err)
}

// SetVersionState moves a version from one state to another. It is a
// compare-and-set: if the stored state is no longer from (a concurrent
// writer won), it returns ErrConflict and the caller's transaction aborts.
func (t *Tx) SetVersionState(ctx context.Context, id string, from, to domain.VersionState) error {
	tag, err := t.db.Exec(ctx,
		`UPDATE model_versions SET state = $3, updated_at = now() WHERE id = $1 AND state = $2`, id, from, to)
	if err != nil {
		return wrap("set version state", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("store: set version %s state %s->%s: %w: state changed concurrently", id, from, to, domain.ErrConflict)
	}
	return nil
}
