// Package stream fans ModelDock's audit events out to live subscribers
// (the Server-Sent Events endpoint). A Tailer reads committed events from
// the audit log in sequence order and publishes them to a Hub; the Hub hands
// each event to every matching subscriber through a bounded channel.
package stream

import (
	"context"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/wenxuewu0216/modeldock/events"
)

// DefaultBuffer is the per-subscriber channel capacity.
const DefaultBuffer = 64

// Metrics are the stream's Prometheus metrics.
type Metrics struct {
	Connections prometheus.Gauge
	EventsSent  prometheus.Counter
	SlowDrops   prometheus.Counter
	GapsSkipped prometheus.Counter
}

// NewMetrics creates the metrics and registers them on r (if non-nil).
func NewMetrics(r prometheus.Registerer) *Metrics {
	m := &Metrics{
		Connections: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "modeldock_sse_connections", Help: "Active Server-Sent Events connections.",
		}),
		EventsSent: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "modeldock_sse_events_sent_total", Help: "Events written to SSE clients (live and replayed).",
		}),
		SlowDrops: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "modeldock_sse_slow_client_drops_total", Help: "SSE subscribers dropped because their buffer was full.",
		}),
		GapsSkipped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "modeldock_sse_tailer_gaps_skipped_total", Help: "Audit sequence gaps the tailer gave up waiting for (rolled-back transactions).",
		}),
	}
	if r != nil {
		r.MustRegister(m.Connections, m.EventsSent, m.SlowDrops, m.GapsSkipped)
	}
	return m
}

// Subscriber receives events from a Hub. Read C until Done is closed. Done
// closes when the subscriber is unsubscribed, dropped for being slow, or the
// hub shuts down; C itself is never closed.
type Subscriber struct {
	C     <-chan events.Event
	c     chan events.Event
	done  chan struct{}
	model string
	// dropped is set (under the hub lock) when the hub evicted this
	// subscriber because its buffer was full.
	dropped bool
}

// Done is closed when the subscription ends for any reason.
func (s *Subscriber) Done() <-chan struct{} { return s.done }

func (s *Subscriber) matches(ev events.Event) bool {
	return s.model == "" || s.model == ev.Model
}

// Hub is an in-process fan-out of audit events. Publish never blocks: a
// subscriber whose buffer is full is dropped and can reconnect with
// Last-Event-ID to replay what it missed from the audit log.
type Hub struct {
	mu     sync.Mutex
	subs   map[*Subscriber]struct{}
	last   int64 // highest sequence published so far (the watermark)
	closed bool
	buffer int
	m      *Metrics
}

// NewHub returns a hub whose subscribers get buffer-sized channels
// (DefaultBuffer if buffer <= 0). m may be nil.
func NewHub(buffer int, m *Metrics) *Hub {
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	if m == nil {
		m = NewMetrics(nil)
	}
	return &Hub{subs: map[*Subscriber]struct{}{}, buffer: buffer, m: m}
}

// Metrics returns the hub's metrics.
func (h *Hub) Metrics() *Metrics { return h.m }

// SetWatermark sets the starting sequence before anything is published.
// The tailer calls it with the audit log's current maximum at startup.
func (h *Hub) SetWatermark(seq int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if seq > h.last {
		h.last = seq
	}
}

// Subscribe registers a subscriber for events of model ("" = all models).
// It returns the subscriber and the watermark at the moment of subscribing:
// every event with Seq greater than the watermark will be delivered on C
// (unless the subscriber is dropped), and every event at or below it has
// already been published, so it is in the audit log for replay. That gives a
// gap-free, duplicate-free hand-over from replay to live.
func (h *Hub) Subscribe(model string) (*Subscriber, int64) {
	c := make(chan events.Event, h.buffer)
	s := &Subscriber{C: c, c: c, done: make(chan struct{}), model: model}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(s.done)
		return s, h.last
	}
	h.subs[s] = struct{}{}
	return s, h.last
}

// SubscribeContext is Subscribe plus automatic Unsubscribe when ctx is
// cancelled (for an HTTP handler: when the client disconnects).
func (h *Hub) SubscribeContext(ctx context.Context, model string) (*Subscriber, int64) {
	s, wm := h.Subscribe(model)
	stop := context.AfterFunc(ctx, func() { h.Unsubscribe(s) })
	go func() {
		<-s.done
		stop()
	}()
	return s, wm
}

// Unsubscribe removes s. It is safe to call more than once and after the
// subscriber was dropped.
func (h *Hub) Unsubscribe(s *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removeLocked(s)
}

func (h *Hub) removeLocked(s *Subscriber) {
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		close(s.done)
	}
}

// Dropped reports whether s was evicted for being too slow.
func (h *Hub) Dropped(s *Subscriber) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return s.dropped
}

// Publish delivers ev to every matching subscriber without blocking. Events
// must be published in increasing Seq order (the Tailer guarantees this).
func (h *Hub) Publish(ev events.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ev.Seq > h.last {
		h.last = ev.Seq
	}
	for s := range h.subs {
		if !s.matches(ev) {
			continue
		}
		select {
		case s.c <- ev:
		default:
			s.dropped = true
			h.removeLocked(s)
			h.m.SlowDrops.Inc()
		}
	}
}

// Watermark returns the highest sequence published so far.
func (h *Hub) Watermark() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}

// Len returns the number of current subscribers.
func (h *Hub) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Close ends every subscription and rejects new ones. It is used on server
// shutdown so long-lived streams do not hold up http.Server.Shutdown.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for s := range h.subs {
		h.removeLocked(s)
	}
}
