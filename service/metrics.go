package service

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the business metrics the service records.
type Metrics struct {
	DeployDuration *prometheus.HistogramVec
	Promotions     *prometheus.CounterVec
	Rollbacks      *prometheus.CounterVec
	Approvals      *prometheus.CounterVec
	HealthChanges  *prometheus.CounterVec
}

// NewMetrics creates and registers the service metrics on r (nil r skips
// registration, useful in tests).
func NewMetrics(r prometheus.Registerer) *Metrics {
	m := &Metrics{
		DeployDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "modeldock_deploy_duration_seconds",
			Help: "Time from a deployment being requested to it becoming ready or failing.",
			// Rollouts take seconds to minutes.
			Buckets: []float64{1, 2.5, 5, 10, 20, 30, 60, 120, 300, 600},
		}, []string{"environment", "kind", "outcome"}),
		Promotions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "modeldock_promotions_total", Help: "Production promotions started.",
		}, []string{"model"}),
		Rollbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "modeldock_rollbacks_total", Help: "Rollbacks started, by trigger (manual|auto).",
		}, []string{"environment", "trigger"}),
		Approvals: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "modeldock_approval_decisions_total", Help: "Promotion approval decisions.",
		}, []string{"decision"}),
		HealthChanges: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "modeldock_inference_health_changes_total", Help: "Inference health transitions observed.",
		}, []string{"environment", "health"}),
	}
	if r != nil {
		r.MustRegister(m.DeployDuration, m.Promotions, m.Rollbacks, m.Approvals, m.HealthChanges)
	}
	return m
}
