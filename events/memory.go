package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
)

// MemoryPublisher records messages in memory. Used by tests and by
// `serve --events=log` for running without Kafka.
type MemoryPublisher struct {
	mu   sync.Mutex
	msgs []Message
	// FailNext makes the next N Publish calls fail (broker outage).
	FailNext int
	// Log, if set, logs each message.
	Log *slog.Logger
}

// Publish implements Publisher.
func (p *MemoryPublisher) Publish(_ context.Context, msgs []Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.FailNext > 0 {
		p.FailNext--
		return fmt.Errorf("memory publisher: simulated broker failure")
	}
	p.msgs = append(p.msgs, msgs...)
	if p.Log != nil {
		for _, m := range msgs {
			p.Log.Info("event", "topic", m.Topic, "key", m.Key, "event_id", m.EventID)
		}
	}
	return nil
}

// Close implements Publisher.
func (p *MemoryPublisher) Close() error { return nil }

// Messages returns a copy of everything published.
func (p *MemoryPublisher) Messages() []Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Message(nil), p.msgs...)
}

// Events decodes the messages published to topic.
func (p *MemoryPublisher) Events(topic string) ([]Event, error) {
	var out []Event
	for _, m := range p.Messages() {
		if m.Topic != topic {
			continue
		}
		var e Event
		if err := json.Unmarshal(m.Value, &e); err != nil {
			return nil, fmt.Errorf("decode %s: %w", m.EventID, err)
		}
		out = append(out, e)
	}
	return out, nil
}
