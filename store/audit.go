package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wenxuewu0216/modeldock/events"
)

// AppendEvent writes ev to the append-only audit log and enqueues it in the
// outbox, inside the caller's transaction. This is the heart of the outbox
// pattern: the event becomes durable if and only if the state change it
// describes commits.
func (t *Tx) AppendEvent(ctx context.Context, ev *events.Event) error {
	var depID *string
	if ev.DeploymentID != "" {
		depID = &ev.DeploymentID
	}
	data := ev.Data
	if data == nil {
		data = json.RawMessage(`{}`)
	}
	err := t.db.QueryRow(ctx, `INSERT INTO audit_events
		(event_id, type, actor, model, version, environment, deployment_id, payload, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING seq`,
		ev.ID, ev.Type, ev.Actor, ev.Model, ev.Version, ev.Environment, depID, data, ev.OccurredAt,
	).Scan(&ev.Seq)
	if err != nil {
		return wrap("append audit event "+ev.Type, err)
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("store: marshal event %s: %w", ev.ID, err)
	}
	_, err = t.db.Exec(ctx, `INSERT INTO outbox (event_id, topic, msg_key, payload) VALUES ($1,$2,$3,$4)`,
		ev.ID, ev.Topic(), ev.Key(), payload)
	return wrap("enqueue outbox "+ev.Type, err)
}

// ListAudit returns audit events newest first. model may be empty. If
// beforeSeq > 0 only events with a lower sequence are returned (pagination).
func (q queries) ListAudit(ctx context.Context, model string, beforeSeq int64, limit int) ([]events.Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.db.Query(ctx, `SELECT seq, event_id, type, actor, model, version, environment,
			COALESCE(deployment_id::text, ''), payload, occurred_at
		FROM audit_events
		WHERE ($1 = '' OR model = $1) AND ($2 = 0 OR seq < $2)
		ORDER BY seq DESC LIMIT $3`, model, beforeSeq, limit)
	if err != nil {
		return nil, wrap("list audit", err)
	}
	evs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (events.Event, error) {
		var e events.Event
		err := r.Scan(&e.Seq, &e.ID, &e.Type, &e.Actor, &e.Model, &e.Version, &e.Environment,
			&e.DeploymentID, &e.Data, &e.OccurredAt)
		return e, err
	})
	return evs, wrap("list audit", err)
}

// ClaimOutbox locks up to limit unpublished outbox rows (in id order) and
// passes them to publish. If publish succeeds the rows are marked published
// in the same transaction; if it fails nothing is marked and the attempt is
// recorded. A crash at any point leaves rows unpublished, so they are
// retried: delivery is at-least-once, never lost.
//
// It returns the number of messages published.
func (s *Store) ClaimOutbox(ctx context.Context, limit int, publish func(context.Context, []events.OutboxMessage) error) (int, error) {
	var msgs []events.OutboxMessage
	var pubErr error
	err := s.InTx(ctx, func(tx *Tx) error {
		rows, err := tx.db.Query(ctx, `SELECT id, event_id, topic, msg_key, payload, created_at
			FROM outbox WHERE published_at IS NULL ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return wrap("claim outbox", err)
		}
		msgs, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (events.OutboxMessage, error) {
			var m events.OutboxMessage
			err := r.Scan(&m.ID, &m.EventID, &m.Topic, &m.Key, &m.Payload, &m.CreatedAt)
			return m, err
		})
		if err != nil {
			return wrap("claim outbox", err)
		}
		if len(msgs) == 0 {
			return nil
		}
		if pubErr = publish(ctx, msgs); pubErr != nil {
			return pubErr
		}
		ids := make([]int64, len(msgs))
		for i, m := range msgs {
			ids[i] = m.ID
		}
		_, err = tx.db.Exec(ctx, `UPDATE outbox SET published_at = now(), attempts = attempts + 1
			WHERE id = ANY($1)`, ids)
		return wrap("mark outbox published", err)
	})
	if pubErr != nil {
		ids := make([]int64, len(msgs))
		for i, m := range msgs {
			ids[i] = m.ID
		}
		// Best effort bookkeeping; the publish error is what matters.
		_, _ = s.pool.Exec(ctx, `UPDATE outbox SET attempts = attempts + 1, last_error = $2 WHERE id = ANY($1)`,
			ids, pubErr.Error())
		return 0, fmt.Errorf("store: publish %d outbox messages: %w", len(msgs), pubErr)
	}
	if err != nil {
		return 0, err
	}
	return len(msgs), nil
}

// OutboxBacklog returns the number of unpublished messages and the age of
// the oldest one (zero if none).
func (q queries) OutboxBacklog(ctx context.Context) (int, time.Duration, error) {
	var n int
	var oldest *time.Time
	err := q.db.QueryRow(ctx, `SELECT count(*), min(created_at) FROM outbox WHERE published_at IS NULL`).Scan(&n, &oldest)
	if err != nil {
		return 0, 0, wrap("outbox backlog", err)
	}
	if oldest == nil {
		return n, 0, nil
	}
	return n, time.Since(*oldest), nil
}
