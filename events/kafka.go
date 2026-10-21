package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/segmentio/kafka-go"
)

// KafkaPublisher publishes to Kafka with acks=all and key-hash partitioning
// (per-model ordering). Messages carry the event ID as a header so
// consumers can deduplicate redeliveries.
type KafkaPublisher struct {
	w *kafka.Writer
}

// NewKafkaPublisher returns a publisher for the given brokers.
func NewKafkaPublisher(brokers []string) *KafkaPublisher {
	return &KafkaPublisher{w: &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Balancer:               &kafka.Hash{},
		RequiredAcks:           kafka.RequireAll,
		BatchTimeout:           5 * time.Millisecond,
		WriteTimeout:           10 * time.Second,
		AllowAutoTopicCreation: false,
		MaxAttempts:            3,
	}}
}

// Publish implements Publisher. It blocks until all messages are acked.
func (p *KafkaPublisher) Publish(ctx context.Context, msgs []Message) error {
	km := make([]kafka.Message, len(msgs))
	for i, m := range msgs {
		km[i] = kafka.Message{
			Topic: m.Topic, Key: []byte(m.Key), Value: m.Value,
			Headers: []kafka.Header{{Key: "event-id", Value: []byte(m.EventID)}},
		}
	}
	if err := p.w.WriteMessages(ctx, km...); err != nil {
		return fmt.Errorf("kafka: write %d messages: %w", len(km), err)
	}
	return nil
}

// Close implements Publisher.
func (p *KafkaPublisher) Close() error { return p.w.Close() }

// EnsureTopics creates ModelDock's topics if they do not exist. The audit
// topic gets infinite retention; domain topics use the broker default.
func EnsureTopics(ctx context.Context, broker string, partitions, replication int) error {
	d := kafka.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", broker)
	if err != nil {
		return fmt.Errorf("kafka: dial %s: %w", broker, err)
	}
	defer conn.Close()
	ctrl, err := conn.Controller()
	if err != nil {
		return fmt.Errorf("kafka: find controller: %w", err)
	}
	cc, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ctrl.Host, strconv.Itoa(ctrl.Port)))
	if err != nil {
		return fmt.Errorf("kafka: dial controller: %w", err)
	}
	defer cc.Close()
	cfgs := make([]kafka.TopicConfig, 0, len(AllTopics))
	for _, t := range AllTopics {
		tc := kafka.TopicConfig{Topic: t, NumPartitions: partitions, ReplicationFactor: replication}
		if t == AuditTopic {
			tc.ConfigEntries = []kafka.ConfigEntry{
				{ConfigName: "retention.ms", ConfigValue: "-1"},
				{ConfigName: "cleanup.policy", ConfigValue: "delete"},
			}
		}
		cfgs = append(cfgs, tc)
	}
	if err := cc.CreateTopics(cfgs...); err != nil && !errors.Is(err, kafka.TopicAlreadyExists) {
		return fmt.Errorf("kafka: create topics: %w", err)
	}
	return nil
}

// DashboardSink is implemented by store.Store.
type DashboardSink interface {
	ApplyDashboardEvent(ctx context.Context, ev Event) (bool, error)
}

// DashboardConsumer reads the audit topic and maintains the health
// dashboard read model. Offsets are committed only after the projection
// commits, and the projection is idempotent per event ID, so the consumer
// is effectively exactly-once even though delivery is at-least-once.
type DashboardConsumer struct {
	Brokers []string
	GroupID string
	Topic   string
	Sink    DashboardSink
	Log     *slog.Logger
}

// Run consumes until ctx is cancelled.
func (c *DashboardConsumer) Run(ctx context.Context) error {
	topic := c.Topic
	if topic == "" {
		topic = AuditTopic
	}
	group := c.GroupID
	if group == "" {
		group = "modeldock-dashboard"
	}
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers: c.Brokers, GroupID: group, Topic: topic,
		MinBytes: 1, MaxBytes: 10e6, MaxWait: 500 * time.Millisecond,
		StartOffset: kafka.FirstOffset,
	})
	defer r.Close()
	for {
		m, err := r.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("dashboard consumer: fetch: %w", err)
		}
		var ev Event
		if err := json.Unmarshal(m.Value, &ev); err != nil {
			// Poison message: log and skip rather than wedge the partition.
			log.Error("dashboard consumer: undecodable message, skipping", "offset", m.Offset, "err", err)
		} else if _, err := c.Sink.ApplyDashboardEvent(ctx, ev); err != nil {
			return fmt.Errorf("dashboard consumer: apply %s: %w", ev.ID, err)
		}
		if err := r.CommitMessages(ctx, m); err != nil {
			return fmt.Errorf("dashboard consumer: commit offset %d: %w", m.Offset, err)
		}
	}
}
