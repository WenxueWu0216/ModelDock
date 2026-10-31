package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/wenxuewu0216/modeldock/auth"
	"github.com/wenxuewu0216/modeldock/domain"
	"github.com/wenxuewu0216/modeldock/events"
	"github.com/wenxuewu0216/modeldock/service"
	"github.com/wenxuewu0216/modeldock/stream"
)

const (
	defaultHeartbeat    = 15 * time.Second
	streamWriteTimeout  = 10 * time.Second
	replayPage          = 500
	defaultReplayMax    = 10000
	clientRetryMillis   = 3000
	streamResetEvent    = "stream.reset"
	streamResetDataJSON = `{"reason":"replay window exceeded; reload current state"}`
)

// streamHandler serves GET /api/v1/events/stream as Server-Sent Events.
type streamHandler struct {
	svc       *service.Service
	hub       *stream.Hub
	log       *slog.Logger
	heartbeat time.Duration
	replayMax int
}

func parseLastEventID(r *http.Request) (int64, bool, error) {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("lastEventId")
	}
	if raw == "" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, false, fmt.Errorf("%w: Last-Event-ID must be a non-negative integer, got %q", domain.ErrInvalid, raw)
	}
	return n, true, nil
}

func (h *streamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	onErr := writeError(h.log)
	ctx := r.Context()
	u, err := auth.CurrentUser(ctx)
	if err != nil {
		onErr(w, r, err)
		return
	}
	model := r.URL.Query().Get("model")
	if err := h.svc.AuthorizeStream(ctx, u, model); err != nil {
		onErr(w, r, err)
		return
	}
	lastID, resume, err := parseLastEventID(r)
	if err != nil {
		onErr(w, r, err)
		return
	}

	// Subscribe before replaying: everything above the watermark arrives on
	// sub.C, everything at or below it is already in the audit log.
	sub, watermark := h.hub.SubscribeContext(ctx, model)
	defer h.hub.Unsubscribe(sub)
	m := h.hub.Metrics()
	m.Connections.Inc()
	defer m.Connections.Dec()

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx)
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	write := func(b []byte) bool {
		if err := rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return false
		}
		if _, err := w.Write(b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	send := func(ev events.Event) bool {
		b, err := stream.Encode(ev)
		if err != nil {
			h.log.Error("event stream: encode", "event_id", ev.ID, "err", err)
			return true
		}
		if !write(b) {
			return false
		}
		m.EventsSent.Inc()
		return true
	}

	if !write([]byte(fmt.Sprintf("retry: %d\n: connected\n\n", clientRetryMillis))) {
		return
	}

	if resume && lastID < watermark {
		cursor, sent := lastID, 0
		for cursor < watermark {
			evs, err := h.svc.ReplayEvents(ctx, u, model, cursor, watermark, replayPage)
			if err != nil {
				if ctx.Err() == nil {
					h.log.Error("event stream: replay", "after", cursor, "err", err)
				}
				return
			}
			for _, ev := range evs {
				if !send(ev) {
					return
				}
			}
			sent += len(evs)
			if len(evs) < replayPage {
				break
			}
			cursor = evs[len(evs)-1].Seq
			if sent >= h.replayMax {
				// Too far behind: tell the client to reload state instead.
				if !write([]byte("event: " + streamResetEvent + "\ndata: " + streamResetDataJSON + "\n\n")) {
					return
				}
				break
			}
		}
	}

	hb := time.NewTicker(h.heartbeat)
	defer hb.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-sub.C:
			if !send(ev) {
				return
			}
		case <-sub.Done():
			// Dropped as a slow client, or server shutdown. Flush what is
			// buffered and end the response; the browser reconnects with
			// Last-Event-ID and replays the rest from the audit log.
			for {
				select {
				case ev := <-sub.C:
					if !send(ev) {
						return
					}
				default:
					return
				}
			}
		case <-hb.C:
			if !write([]byte(": ping\n\n")) {
				return
			}
		}
	}
}
