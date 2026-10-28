package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/wenxuewu0216/modeldock/domain"
)

const approvalCols = `a.id, a.version_id, m.name, v.version, a.status, a.requested_by, a.requested_at,
	COALESCE(a.decided_by, ''), a.decided_at, a.comment`

const approvalFrom = ` FROM approvals a
	JOIN model_versions v ON v.id = a.version_id
	JOIN models m ON m.id = v.model_id `

func scanApproval(r pgx.Row) (domain.Approval, error) {
	var a domain.Approval
	err := r.Scan(&a.ID, &a.VersionID, &a.ModelName, &a.Version, &a.Status, &a.RequestedBy, &a.RequestedAt,
		&a.DecidedBy, &a.DecidedAt, &a.Comment)
	return a, err
}

// CreateApproval records a pending promotion request. A partial unique index
// guarantees at most one pending request per version.
func (t *Tx) CreateApproval(ctx context.Context, a *domain.Approval) error {
	a.ID = uuid.NewString()
	a.Status = domain.ApprovalPending
	err := t.db.QueryRow(ctx, `INSERT INTO approvals (id, version_id, status, requested_by, comment)
		VALUES ($1,$2,$3,$4,$5) RETURNING requested_at`,
		a.ID, a.VersionID, a.Status, a.RequestedBy, a.Comment).Scan(&a.RequestedAt)
	return wrap("create approval", err)
}

// GetApproval returns an approval by ID.
func (q queries) GetApproval(ctx context.Context, id string) (domain.Approval, error) {
	if _, err := uuid.Parse(id); err != nil {
		return domain.Approval{}, fmt.Errorf("store: get approval %q: %w", id, domain.ErrNotFound)
	}
	a, err := scanApproval(q.db.QueryRow(ctx, `SELECT `+approvalCols+approvalFrom+`WHERE a.id = $1`, id))
	return a, wrap("get approval "+id, err)
}

// LatestApproval returns the most recent approval for a version.
func (q queries) LatestApproval(ctx context.Context, versionID string) (domain.Approval, error) {
	a, err := scanApproval(q.db.QueryRow(ctx, `SELECT `+approvalCols+approvalFrom+`
		WHERE a.version_id = $1 ORDER BY a.requested_at DESC LIMIT 1`, versionID))
	return a, wrap("latest approval for version "+versionID, err)
}

// ListApprovals returns approvals, newest first, optionally filtered by status.
func (q queries) ListApprovals(ctx context.Context, status domain.ApprovalStatus) ([]domain.Approval, error) {
	rows, err := q.db.Query(ctx, `SELECT `+approvalCols+approvalFrom+`
		WHERE ($1 = '' OR a.status = $1) ORDER BY a.requested_at DESC LIMIT 200`, string(status))
	if err != nil {
		return nil, wrap("list approvals", err)
	}
	as, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Approval, error) { return scanApproval(r) })
	return as, wrap("list approvals", err)
}

// DecideApproval moves a pending approval to approved/rejected. It only
// matches pending rows, so two approvers racing cannot both decide.
func (t *Tx) DecideApproval(ctx context.Context, id string, status domain.ApprovalStatus, by, comment string) (domain.Approval, error) {
	tag, err := t.db.Exec(ctx, `UPDATE approvals SET status = $2, decided_by = $3, decided_at = now(),
			comment = CASE WHEN $4 = '' THEN comment ELSE $4 END
		WHERE id = $1 AND status = 'pending'`, id, status, by, comment)
	if err != nil {
		return domain.Approval{}, wrap("decide approval "+id, err)
	}
	if tag.RowsAffected() != 1 {
		return domain.Approval{}, fmt.Errorf("store: decide approval %s: %w: not pending", id, domain.ErrConflict)
	}
	return t.GetApproval(ctx, id)
}
