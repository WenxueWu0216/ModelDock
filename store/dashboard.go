package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/wenxuewu0216/modeldock/events"
)

const healthCols = `model, last_deploy_version, last_deploy_env, last_deploy_status, last_deploy_at,
	deploys_total, deploys_failed, rollbacks_total, promotions_total, last_health, last_health_at, updated_at`

func scanHealth(r pgx.Row) (events.ModelHealth, error) {
	var h events.ModelHealth
	err := r.Scan(&h.Model, &h.LastDeployVersion, &h.LastDeployEnv, &h.LastDeployStatus, &h.LastDeployAt,
		&h.DeploysTotal, &h.DeploysFailed, &h.RollbacksTotal, &h.PromotionsTotal, &h.LastHealth,
		&h.LastHealthAt, &h.UpdatedAt)
	return h, err
}

// ApplyDashboardEvent folds ev into the dashboard read model exactly once:
// the event ID is recorded in the same transaction as the projection, so a
// redelivered event (at-least-once Kafka) is a no-op. It returns false if
// the event had already been processed.
func (s *Store) ApplyDashboardEvent(ctx context.Context, ev events.Event) (bool, error) {
	applied := false
	err := s.InTx(ctx, func(tx *Tx) error {
		tag, err := tx.db.Exec(ctx, `INSERT INTO dashboard_processed_events (event_id) VALUES ($1)
			ON CONFLICT DO NOTHING`, ev.ID)
		if err != nil {
			return wrap("record processed event", err)
		}
		if tag.RowsAffected() == 0 {
			return nil // duplicate delivery
		}
		h, err := scanHealth(tx.db.QueryRow(ctx, `SELECT `+healthCols+` FROM dashboard_model_health
			WHERE model = $1 FOR UPDATE`, ev.Model))
		if err != nil && err != pgx.ErrNoRows {
			return wrap("load model health", err)
		}
		if !events.Project(&h, ev) {
			return nil
		}
		applied = true
		_, err = tx.db.Exec(ctx, `INSERT INTO dashboard_model_health (`+healthCols+`)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			ON CONFLICT (model) DO UPDATE SET
				last_deploy_version = EXCLUDED.last_deploy_version, last_deploy_env = EXCLUDED.last_deploy_env,
				last_deploy_status = EXCLUDED.last_deploy_status, last_deploy_at = EXCLUDED.last_deploy_at,
				deploys_total = EXCLUDED.deploys_total, deploys_failed = EXCLUDED.deploys_failed,
				rollbacks_total = EXCLUDED.rollbacks_total, promotions_total = EXCLUDED.promotions_total,
				last_health = EXCLUDED.last_health, last_health_at = EXCLUDED.last_health_at,
				updated_at = EXCLUDED.updated_at`,
			h.Model, h.LastDeployVersion, h.LastDeployEnv, h.LastDeployStatus, h.LastDeployAt,
			h.DeploysTotal, h.DeploysFailed, h.RollbacksTotal, h.PromotionsTotal, h.LastHealth,
			h.LastHealthAt, h.UpdatedAt)
		return wrap("upsert model health", err)
	})
	return applied, err
}

// ListDashboard returns the dashboard read model.
func (q queries) ListDashboard(ctx context.Context) ([]events.ModelHealth, error) {
	rows, err := q.db.Query(ctx, `SELECT `+healthCols+` FROM dashboard_model_health ORDER BY model`)
	if err != nil {
		return nil, wrap("list dashboard", err)
	}
	hs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (events.ModelHealth, error) { return scanHealth(r) })
	return hs, wrap("list dashboard", err)
}
