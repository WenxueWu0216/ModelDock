package stream

import (
	"context"
	"log/slog"
	"time"

	"github.com/wenxuewu0216/modeldock/events"
)

// AuditSource is implemented by store.Store.
type AuditSource interface {
	ListAuditAfter(ctx context.Context, model string, afterSeq, uptoSeq int64, limit int) ([]events.Event, error)
	MaxAuditSeq(ctx context.Context) (int64, error)
}

// Tailer polls the audit log and publishes newly committed events to a Hub
// in strict sequence order.
//
// audit_events.seq is a bigserial, assigned at INSERT time, so transactions
// can commit out of sequence order: seq 51 may be visible while seq 50 is
// still in flight. Reading "everything after the last seq I saw" would then
// skip 50 for good. The tailer instead keeps a cursor below which every
// sequence is resolved, and holds events back behind a gap until the gap
// fills or GapTimeout passes (a rolled-back transaction leaves a permanent
// gap, because sequences are not transactional).
type Tailer struct {
	Source AuditSource
	Hub    *Hub
	// Interval between polls (default 200ms).
	Interval time.Duration
	// GapTimeout is how long to wait for a missing sequence number before
	// skipping it (default 3s).
	GapTimeout time.Duration
	// BatchSize caps rows per query (default 500).
	BatchSize int
	Log       *slog.Logger

	now      func() time.Time
	cursor   int64
	gapSince map[int64]time.Time
	inited   bool
}

// Init positions the tailer at the end of the audit log and sets the hub's
// watermark. Call it before serving stream requests so replay and live
// delivery agree on where "now" is.
func (t *Tailer) Init(ctx context.Context) error {
	seq, err := t.Source.MaxAuditSeq(ctx)
	if err != nil {
		return err
	}
	t.cursor = seq
	t.gapSince = map[int64]time.Time{}
	if t.now == nil {
		t.now = time.Now
	}
	t.inited = true
	t.Hub.SetWatermark(seq)
	return nil
}

// Cursor returns the highest sequence published (or skipped) so far.
func (t *Tailer) Cursor() int64 { return t.cursor }

// PollOnce publishes every event that is now in order. It returns how many
// events were published. It is not safe for concurrent use; Run calls it
// from one goroutine.
func (t *Tailer) PollOnce(ctx context.Context) (int, error) {
	if !t.inited {
		if err := t.Init(ctx); err != nil {
			return 0, err
		}
	}
	batch := t.BatchSize
	if batch <= 0 {
		batch = 500
	}
	gapTimeout := t.GapTimeout
	if gapTimeout <= 0 {
		gapTimeout = 3 * time.Second
	}
	published := 0
	for {
		rows, err := t.Source.ListAuditAfter(ctx, "", t.cursor, 0, batch)
		if err != nil {
			return published, err
		}
		n, blocked := t.advance(rows, gapTimeout)
		published += n
		if blocked || len(rows) < batch {
			return published, nil
		}
	}
}

// advance publishes the in-order prefix of rows (sorted by seq, all greater
// than the cursor). It reports whether it stopped at an unresolved gap.
func (t *Tailer) advance(rows []events.Event, gapTimeout time.Duration) (int, bool) {
	published := 0
	for i := 0; i < len(rows); {
		next := t.cursor + 1
		ev := rows[i]
		if ev.Seq == next {
			t.Hub.Publish(ev)
			t.cursor = next
			delete(t.gapSince, next)
			published++
			i++
			continue
		}
		// ev.Seq > next: sequence `next` is not visible (yet).
		first, seen := t.gapSince[next]
		if !seen {
			t.gapSince[next] = t.now()
			return published, true
		}
		if t.now().Sub(first) < gapTimeout {
			return published, true
		}
		// Give up on `next`: almost certainly a rolled-back transaction.
		delete(t.gapSince, next)
		t.cursor = next
		t.Hub.Metrics().GapsSkipped.Inc()
		if t.Log != nil {
			t.Log.Warn("event stream: skipped audit sequence gap", "seq", next, "waited", t.now().Sub(first))
		}
	}
	return published, false
}

// Run polls until ctx is cancelled.
func (t *Tailer) Run(ctx context.Context) {
	interval := t.Interval
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	log := t.Log
	if log == nil {
		log = slog.Default()
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if _, err := t.PollOnce(ctx); err != nil && ctx.Err() == nil {
			log.Warn("event stream: poll audit log failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
