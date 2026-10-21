package events

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// OutboxMessage is one unpublished outbox row.
type OutboxMessage struct {
	ID        int64
	EventID   string
	Topic     string
	Key       string
	Payload   []byte
	CreatedAt time.Time
}

// Message is what a Publisher sends: one Kafka record.
type Message struct {
	Topic   string
	Key     string
	Value   []byte
	EventID string
}

// Publisher delivers messages to the broker. Publish must return nil only
// once every message is durably acknowledged.
type Publisher interface {
	Publish(ctx context.Context, msgs []Message) error
	Close() error
}

// OutboxSource is implemented by store.Store.
type OutboxSource interface {
	ClaimOutbox(ctx context.Context, limit int, publish func(context.Context, []OutboxMessage) error) (int, error)
	OutboxBacklog(ctx context.Context) (int, time.Duration, error)
}

// LeaderLock is implemented by store.Store (Postgres advisory lock).
type LeaderLock interface {
	TryLeader(ctx context.Context, key int64) (release func(), ok bool, err error)
}

// RelayLockKey is the advisory-lock key that elects the single active relay.
// One relay preserves per-model ordering; SKIP LOCKED in ClaimOutbox keeps
// it safe even if two ever ran.
const RelayLockKey int64 = 0x6d6f64656c01 // "model" + 1

var (
	outboxPublished = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "modeldock_outbox_published_total", Help: "Outbox messages published to Kafka.",
	})
	outboxErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "modeldock_outbox_publish_errors_total", Help: "Failed outbox publish batches.",
	})
	outboxBacklog = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "modeldock_outbox_backlog", Help: "Unpublished outbox messages.",
	})
	outboxLag = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "modeldock_outbox_oldest_age_seconds", Help: "Age of the oldest unpublished outbox message.",
	})
)

// RegisterMetrics registers the outbox metrics on r.
func RegisterMetrics(r prometheus.Registerer) {
	r.MustRegister(outboxPublished, outboxErrors, outboxBacklog, outboxLag)
}

// Relay moves events from the outbox table to Kafka.
type Relay struct {
	Source    OutboxSource
	Lock      LeaderLock
	Publisher Publisher
	Interval  time.Duration
	BatchSize int
	Log       *slog.Logger
	// AuditTopic, if set, receives a copy of every message (immutable audit
	// stream). Defaults to events.AuditTopic; set to "-" to disable.
	AuditTopic string
}

// Expand turns outbox rows into Kafka messages: each row goes to its domain
// topic and to the audit topic.
func (r *Relay) Expand(rows []OutboxMessage) []Message {
	audit := r.AuditTopic
	if audit == "" {
		audit = AuditTopic
	}
	out := make([]Message, 0, 2*len(rows))
	for _, m := range rows {
		out = append(out, Message{Topic: m.Topic, Key: m.Key, Value: m.Payload, EventID: m.EventID})
		if audit != "-" && m.Topic != audit {
			out = append(out, Message{Topic: audit, Key: m.Key, Value: m.Payload, EventID: m.EventID})
		}
	}
	return out
}

// PublishOnce drains up to one batch. It returns how many outbox rows were
// published.
func (r *Relay) PublishOnce(ctx context.Context) (int, error) {
	n, err := r.Source.ClaimOutbox(ctx, r.batch(), func(ctx context.Context, rows []OutboxMessage) error {
		return r.Publisher.Publish(ctx, r.Expand(rows))
	})
	if err != nil {
		outboxErrors.Inc()
		return 0, fmt.Errorf("relay: %w", err)
	}
	outboxPublished.Add(float64(n))
	return n, nil
}

// Drain publishes until the outbox is empty or an error occurs.
func (r *Relay) Drain(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := r.PublishOnce(ctx)
		total += n
		if err != nil || n == 0 {
			return total, err
		}
	}
}

func (r *Relay) batch() int {
	if r.BatchSize <= 0 {
		return 100
	}
	return r.BatchSize
}

// Run loops until ctx is cancelled. Only the replica holding the advisory
// lock publishes; others retry acquiring it every interval.
func (r *Relay) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	log := r.Log
	if log == nil {
		log = slog.Default()
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	for {
		if release == nil && r.Lock != nil {
			rel, ok, err := r.Lock.TryLeader(ctx, RelayLockKey)
			if err != nil && ctx.Err() == nil {
				log.Warn("outbox relay: leader election failed", "err", err)
			}
			if ok {
				release = rel
				log.Info("outbox relay: acquired leadership")
			}
		}
		if release != nil || r.Lock == nil {
			if _, err := r.Drain(ctx); err != nil && ctx.Err() == nil {
				log.Warn("outbox relay: publish failed, will retry", "err", err)
			}
			if n, age, err := r.Source.OutboxBacklog(ctx); err == nil {
				outboxBacklog.Set(float64(n))
				outboxLag.Set(age.Seconds())
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
