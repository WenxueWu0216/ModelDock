package events_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/wenxuewu0216/modeldock/domain"
	"github.com/wenxuewu0216/modeldock/events"
	"github.com/wenxuewu0216/modeldock/service"
	"github.com/wenxuewu0216/modeldock/store/storetest"
)

// TestKafkaEndToEnd: service -> outbox -> relay -> Kafka -> dashboard
// consumer -> Postgres read model. Skipped unless MODELDOCK_TEST_KAFKA_BROKERS
// is set (e.g. localhost:29092 with `make compose-up`).
func TestKafkaEndToEnd(t *testing.T) {
	brokers := os.Getenv("MODELDOCK_TEST_KAFKA_BROKERS")
	if brokers == "" {
		t.Skip("MODELDOCK_TEST_KAFKA_BROKERS not set; skipping Kafka integration test")
	}
	bs := strings.Split(brokers, ",")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := events.EnsureTopics(ctx, bs[0], 3, 1); err != nil {
		t.Fatal(err)
	}

	s := storetest.New(t)
	svc := service.New(s, nil, nil, false)
	model := fmt.Sprintf("iris-%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := svc.CreateModel(ctx, deployer, model, ""); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"1", "2"} {
		if _, err := svc.RegisterVersion(ctx, deployer, model, domain.ModelVersion{Version: v, Image: "img:" + v}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.DeployToStaging(ctx, deployer, model, v); err != nil {
			t.Fatal(err)
		}
	}
	audit, err := s.ListAudit(ctx, model, 0, 100)
	if err != nil {
		t.Fatal(err)
	}

	pub := events.NewKafkaPublisher(bs)
	defer pub.Close()
	relay := &events.Relay{Source: s, Publisher: pub}
	n, err := relay.Drain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(audit) {
		t.Fatalf("published %d outbox rows, want %d", n, len(audit))
	}
	// Simulate an at-least-once redelivery of the whole batch.
	var dup []events.Message
	for _, ev := range audit {
		b, _ := jsonMarshal(ev)
		dup = append(dup, events.Message{Topic: events.AuditTopic, Key: ev.Key(), Value: b, EventID: ev.ID})
	}
	if err := pub.Publish(ctx, dup); err != nil {
		t.Fatal(err)
	}

	// Every event is on the audit topic, in commit order for this model.
	seen := readTopic(ctx, t, bs, events.AuditTopic, model, 2*len(audit))
	for i, ev := range reverse(audit) {
		if seen[i] != ev.ID {
			t.Fatalf("audit topic order: position %d = %s, want %s", i, seen[i], ev.ID)
		}
	}

	// The consumer builds the dashboard exactly once despite duplicates.
	cctx, ccancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	c := &events.DashboardConsumer{Brokers: bs, GroupID: "it-" + model, Sink: s}
	go func() { done <- c.Run(cctx) }()
	deadline := time.Now().Add(60 * time.Second)
	for {
		rows, err := s.ListDashboard(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var got *events.ModelHealth
		for i := range rows {
			if rows[i].Model == model {
				got = &rows[i]
			}
		}
		if got != nil && got.LastDeployVersion == "2" {
			if got.LastDeployStatus != "pending" || got.DeploysTotal != 0 {
				t.Fatalf("dashboard row = %+v", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dashboard not built in time: %+v", rows)
		}
		time.Sleep(500 * time.Millisecond)
	}
	ccancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func reverse(evs []events.Event) []events.Event {
	out := make([]events.Event, len(evs))
	for i, e := range evs {
		out[len(evs)-1-i] = e
	}
	return out
}

// readTopic returns the event IDs for key=model on topic, in offset order,
// reading until want messages for that key were seen.
func readTopic(ctx context.Context, t *testing.T, brokers []string, topic, model string, want int) []string {
	t.Helper()
	conn, err := kafka.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		t.Fatal(err)
	}
	parts, err := conn.ReadPartitions(topic)
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range parts {
		r := kafka.NewReader(kafka.ReaderConfig{Brokers: brokers, Topic: topic, Partition: p.ID, MaxWait: 200 * time.Millisecond})
		_ = r.SetOffset(kafka.FirstOffset)
		for {
			rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			m, err := r.ReadMessage(rctx)
			cancel()
			if err != nil {
				break
			}
			if string(m.Key) == model {
				for _, h := range m.Headers {
					if h.Key == "event-id" {
						ids = append(ids, string(h.Value))
					}
				}
			}
		}
		r.Close()
	}
	if len(ids) != want {
		t.Fatalf("read %d messages for %s on %s, want %d", len(ids), model, topic, want)
	}
	return ids
}

func jsonMarshal(ev events.Event) ([]byte, error) { return json.Marshal(ev) }
