package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wenxuewu0216/modeldock/domain"
	"github.com/wenxuewu0216/modeldock/stream"
)

// sseFrame is one parsed Server-Sent Events frame.
type sseFrame struct {
	id, event, data string
	comment         string
}

type sseClient struct {
	t      *testing.T
	resp   *http.Response
	frames chan sseFrame
	cancel context.CancelFunc
}

// openStream connects to the event stream. hdr may set Authorization or
// Last-Event-ID; query is appended to the URL.
func openStream(t *testing.T, base, query string, hdr map[string]string) (*sseClient, int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/v1/events/stream"+query, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		cancel()
		return nil, resp.StatusCode
	}
	c := &sseClient{t: t, resp: resp, frames: make(chan sseFrame, 256), cancel: cancel}
	go func() {
		defer close(c.frames)
		sc := bufio.NewScanner(resp.Body)
		var f sseFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				c.frames <- f
				f = sseFrame{}
			case strings.HasPrefix(line, ":"):
				f.comment = strings.TrimSpace(line[1:])
			case strings.HasPrefix(line, "id: "):
				f.id = line[4:]
			case strings.HasPrefix(line, "event: "):
				f.event = line[7:]
			case strings.HasPrefix(line, "data: "):
				f.data = line[6:]
			}
		}
	}()
	t.Cleanup(c.close)
	return c, 200
}

func (c *sseClient) close() {
	c.cancel()
	c.resp.Body.Close()
}

// nextEvent returns the next frame that carries an event, skipping comments.
func (c *sseClient) nextEvent() (sseFrame, stream.Message) {
	c.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-c.frames:
			if !ok {
				c.t.Fatal("stream closed")
			}
			if f.event == "" {
				continue
			}
			var m stream.Message
			if err := json.Unmarshal([]byte(f.data), &m); err != nil {
				c.t.Fatalf("decode %q: %v", f.data, err)
			}
			return f, m
		case <-deadline:
			c.t.Fatal("timed out waiting for an event")
		}
	}
}

func (c *sseClient) expectTypes(types ...string) []stream.Message {
	c.t.Helper()
	var out []stream.Message
	for _, want := range types {
		f, m := c.nextEvent()
		if f.event != want || m.Type != want {
			c.t.Fatalf("event = %s (%+v), want %s", f.event, m, want)
		}
		if f.id != strconv.FormatInt(m.Seq, 10) {
			c.t.Fatalf("id %s != seq %d", f.id, m.Seq)
		}
		out = append(out, m)
	}
	return out
}

func registerIris(t *testing.T, dep client) {
	dep.expect(201, "POST", "/api/v1/models", map[string]string{"name": "iris"}, nil)
	dep.expect(201, "POST", "/api/v1/models/iris/versions", map[string]any{
		"version": "1.0.0", "image": "ghcr.io/acme/iris:1.0.0",
		"resources": map[string]string{"cpuRequest": "100m", "memoryRequest": "128Mi"},
	}, nil)
}

func TestEventStreamLiveRollout(t *testing.T) {
	h := newHarness(t)
	dep := h.as("dana", domain.RoleDeployer)
	registerIris(t, dep)
	h.caughtUp()

	// The browser passes the token as a query parameter (EventSource
	// cannot set headers).
	sc, code := openStream(t, h.srv.URL, "?model=iris&access_token="+h.token("vic", domain.RoleViewer), nil)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if ct := sc.resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	if cc := sc.resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("cache-control %q", cc)
	}

	var d domain.Deployment
	dep.expect(202, "POST", "/api/v1/models/iris/versions/1.0.0/deploy", map[string]string{"environment": "staging"}, &d)
	h.reconcile(t)

	ms := sc.expectTypes("deploy.requested", "deploy.progressing", "deploy.ready")
	for i, want := range []string{"pending", "progressing", "ready"} {
		m := ms[i]
		if m.Status != want || m.DeploymentID != d.ID || m.Revision != 1 || m.Kind != "deploy" ||
			m.Model != "iris" || m.Environment != "staging" || m.Version != "1.0.0" {
			t.Fatalf("event %d = %+v", i, m)
		}
	}
	if ms[0].Actor != "dana" || ms[2].Actor != "system:reconciler" {
		t.Fatalf("actors = %s, %s", ms[0].Actor, ms[2].Actor)
	}
	if !(ms[0].Seq < ms[1].Seq && ms[1].Seq < ms[2].Seq) {
		t.Fatalf("seqs not increasing: %d %d %d", ms[0].Seq, ms[1].Seq, ms[2].Seq)
	}

	// Approval events reach the stream too.
	var a domain.Approval
	dep.expect(201, "POST", "/api/v1/models/iris/versions/1.0.0/promotion-requests", nil, &a)
	_, m := sc.nextEvent()
	if m.Type != "approval.requested" || m.ApprovalID != a.ID {
		t.Fatalf("approval event = %+v", m)
	}
}

func TestEventStreamModelFilter(t *testing.T) {
	h := newHarness(t)
	dep := h.as("dana", domain.RoleDeployer)
	dep.expect(201, "POST", "/api/v1/models", map[string]string{"name": "iris"}, nil)
	h.caughtUp()
	sc, _ := openStream(t, h.srv.URL, "?model=iris", map[string]string{"Authorization": "Bearer " + h.token("vic", domain.RoleViewer)})
	dep.expect(201, "POST", "/api/v1/models", map[string]string{"name": "wine"}, nil)
	dep.expect(201, "POST", "/api/v1/models/iris/versions", map[string]any{
		"version": "1.0.0", "image": "ghcr.io/acme/iris:1.0.0",
		"resources": map[string]string{"cpuRequest": "100m", "memoryRequest": "128Mi"},
	}, nil)
	_, m := sc.nextEvent()
	if m.Model != "iris" || m.Type != "registry.version_registered" {
		t.Fatalf("got %+v; wine's event should be filtered out", m)
	}
}

func TestEventStreamResumeWithLastEventID(t *testing.T) {
	h := newHarness(t)
	dep := h.as("dana", domain.RoleDeployer)
	registerIris(t, dep)

	// Everything happens while no client is connected.
	dep.expect(202, "POST", "/api/v1/models/iris/versions/1.0.0/deploy", nil, nil)
	h.reconcile(t)
	h.caughtUp()
	var audit []struct {
		Seq  int64  `json:"seq"`
		Type string `json:"type"`
	}
	dep.expect(200, "GET", "/api/v1/audit?model=iris", nil, &audit)
	if len(audit) != 5 { // model, version, requested, progressing, ready
		t.Fatalf("audit = %+v", audit)
	}
	// Pretend the client last saw the version registration (audit is newest first).
	last := audit[3].Seq
	tok := h.token("vic", domain.RoleViewer)
	sc, code := openStream(t, h.srv.URL, "", map[string]string{"Authorization": "Bearer " + tok,
		"Last-Event-ID": strconv.FormatInt(last, 10)})
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	ms := sc.expectTypes("deploy.requested", "deploy.progressing", "deploy.ready")
	if ms[0].Seq != audit[2].Seq || ms[2].Seq != audit[0].Seq {
		t.Fatalf("replayed %+v, audit %+v", ms, audit)
	}
	// Then it switches to live without a gap or a duplicate.
	dep.expect(201, "POST", "/api/v1/models/iris/versions/1.0.0/promotion-requests", nil, nil)
	_, m := sc.nextEvent()
	if m.Type != "approval.requested" || m.Seq <= ms[2].Seq {
		t.Fatalf("live event after replay = %+v", m)
	}

	// The query-parameter form works too (EventSource reconnects send the
	// header, but a fresh page load can only use the URL).
	sc2, _ := openStream(t, h.srv.URL, "?access_token="+tok+"&lastEventId="+strconv.FormatInt(audit[1].Seq, 10), nil)
	sc2.expectTypes("deploy.ready", "approval.requested")
}

func TestEventStreamAuthAndValidation(t *testing.T) {
	h := newHarness(t)
	view := h.token("vic", domain.RoleViewer)
	cases := []struct {
		name  string
		query string
		hdr   map[string]string
		want  int
	}{
		{"no token", "", nil, 401},
		{"bad query token", "?access_token=nope", nil, 401},
		{"bad header token", "", map[string]string{"Authorization": "Bearer nope"}, 401},
		// Header takes precedence: a bad header is not rescued by a good query token.
		{"bad header good query", "?access_token=" + view, map[string]string{"Authorization": "Basic x"}, 401},
		// No role below viewer exists; an unknown role is rejected at verification.
		{"unknown role", "?access_token=" + h.token("eve", domain.Role("guest")), nil, 401},
		{"unknown model", "?model=nope&access_token=" + view, nil, 404},
		{"bad last-event-id", "?lastEventId=abc&access_token=" + view, nil, 400},
	}
	for _, c := range cases {
		if _, code := openStream(t, h.srv.URL, c.query, c.hdr); code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, code, c.want)
		}
	}
	// Every real role (viewer and up) may subscribe.
	for _, r := range []domain.Role{domain.RoleViewer, domain.RoleDeployer, domain.RoleApprover, domain.RoleAdmin} {
		if _, code := openStream(t, h.srv.URL, "?access_token="+h.token("u", r), nil); code != 200 {
			t.Errorf("role %s: status %d", r, code)
		}
	}
	// Query tokens are accepted only on the stream endpoint.
	c := client{t: t, base: h.srv.URL}
	c.expect(401, "GET", "/api/v1/models?access_token="+view, nil, nil)
}

func TestEventStreamHeartbeatAndMetrics(t *testing.T) {
	h := newHarness(t)
	sc, _ := openStream(t, h.srv.URL, "?access_token="+h.token("vic", domain.RoleViewer), nil)
	deadline := time.After(3 * time.Second)
	for got := false; !got; {
		select {
		case f := <-sc.frames:
			got = f.comment == "ping"
		case <-deadline:
			t.Fatal("no heartbeat")
		}
	}
	if n := testutil.ToFloat64(h.hub.Metrics().Connections); n != 1 {
		t.Fatalf("connections = %v, want 1", n)
	}
	h.as("dana", domain.RoleDeployer).expect(201, "POST", "/api/v1/models", map[string]string{"name": "iris"}, nil)
	sc.nextEvent()
	for i := 0; i < 100 && testutil.ToFloat64(h.hub.Metrics().EventsSent) < 1; i++ {
		time.Sleep(10 * time.Millisecond) // the counter is bumped after the flush
	}
	if n := testutil.ToFloat64(h.hub.Metrics().EventsSent); n != 1 {
		t.Fatalf("events sent = %v, want 1", n)
	}
	sc.close()
	for i := 0; i < 100 && (h.hub.Len() != 0 || testutil.ToFloat64(h.hub.Metrics().Connections) != 0); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if h.hub.Len() != 0 || testutil.ToFloat64(h.hub.Metrics().Connections) != 0 {
		t.Fatalf("subscriber not cleaned up after disconnect: subs=%d", h.hub.Len())
	}
	_, body := client{t: t, base: h.srv.URL}.do("GET", "/metrics", nil)
	for _, name := range []string{"modeldock_sse_connections", "modeldock_sse_events_sent_total", "modeldock_sse_slow_client_drops_total"} {
		if !strings.Contains(string(body), name) {
			t.Errorf("/metrics lacks %s", name)
		}
	}
}

func TestCORS(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest("OPTIONS", h.srv.URL+"/api/v1/models", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "http://localhost:5173" ||
		!strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("preflight: %d %v", resp.StatusCode, resp.Header)
	}
	req, _ = http.NewRequest("GET", h.srv.URL+"/healthz", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("disallowed origin got CORS headers")
	}
}
