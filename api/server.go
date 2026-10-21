package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/wenxuewu0216/modeldock/auth"
	"github.com/wenxuewu0216/modeldock/service"
)

// HTTPMetrics instruments the API.
type HTTPMetrics struct {
	latency  *prometheus.HistogramVec
	inflight prometheus.Gauge
}

// NewHTTPMetrics creates and registers HTTP metrics on r.
func NewHTTPMetrics(r prometheus.Registerer) *HTTPMetrics {
	m := &HTTPMetrics{
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "modeldock_http_request_duration_seconds",
			Help:    "API request latency by route, method and status code.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		}, []string{"route", "method", "code"}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "modeldock_http_requests_in_flight", Help: "In-flight API requests.",
		}),
	}
	if r != nil {
		r.MustRegister(m.latency, m.inflight)
	}
	return m
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

// routeMux registers handlers with a stable route label for metrics
// (http.Request.Pattern needs Go 1.23; this keeps us on 1.22).
type routeMux struct {
	*http.ServeMux
	metrics *HTTPMetrics
}

func (m *routeMux) handle(pattern, route string, h http.Handler) {
	if m.metrics == nil {
		m.Handle(pattern, h)
		return
	}
	m.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.metrics.inflight.Inc()
		defer m.metrics.inflight.Dec()
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		h.ServeHTTP(rec, r)
		m.metrics.latency.WithLabelValues(route, r.Method, strconv.Itoa(rec.code)).Observe(time.Since(start).Seconds())
	}))
}

// Config wires the API server.
type Config struct {
	Service  *service.Service
	Auth     *auth.Authenticator
	Metrics  *HTTPMetrics
	Gatherer prometheus.Gatherer
	Log      *slog.Logger
	// Ready reports dependency health for /readyz.
	Ready func(ctx context.Context) error
	// Playground serves the GraphQL playground at /playground.
	Playground bool
}

// NewHandler builds the full HTTP handler: unauthenticated health and
// metrics endpoints, and authenticated REST and GraphQL APIs.
func NewHandler(cfg Config) http.Handler {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	root.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if cfg.Ready != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := cfg.Ready(ctx); err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "error": err.Error()})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	if cfg.Gatherer != nil {
		root.Handle("GET /metrics", promhttp.HandlerFor(cfg.Gatherer, promhttp.HandlerOpts{}))
	}

	authed := &routeMux{ServeMux: http.NewServeMux(), metrics: cfg.Metrics}
	(&rest{svc: cfg.Service, log: log}).routes(authed)
	authed.handle("POST /graphql", "graphql", newGraphQL(cfg.Service))
	authed.handle("GET /graphql", "graphql", newGraphQL(cfg.Service))

	mw := cfg.Auth.Middleware(writeError(log))
	root.Handle("/api/", mw(authed))
	root.Handle("/graphql", mw(authed))
	if cfg.Playground {
		root.Handle("GET /playground", playgroundHandler())
	}
	return root
}
