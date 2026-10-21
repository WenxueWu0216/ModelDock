package events

import (
	"encoding/json"
	"time"
)

// ModelHealth is one row of the health dashboard read model.
type ModelHealth struct {
	Model             string     `json:"model"`
	LastDeployVersion string     `json:"lastDeployVersion"`
	LastDeployEnv     string     `json:"lastDeployEnv"`
	LastDeployStatus  string     `json:"lastDeployStatus"`
	LastDeployAt      *time.Time `json:"lastDeployAt,omitempty"`
	DeploysTotal      int        `json:"deploysTotal"`
	DeploysFailed     int        `json:"deploysFailed"`
	RollbacksTotal    int        `json:"rollbacksTotal"`
	PromotionsTotal   int        `json:"promotionsTotal"`
	LastHealth        string     `json:"lastHealth"`
	LastHealthAt      *time.Time `json:"lastHealthAt,omitempty"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

// FailureRate is failed rollouts / finished rollouts.
func (h ModelHealth) FailureRate() float64 {
	if h.DeploysTotal == 0 {
		return 0
	}
	return float64(h.DeploysFailed) / float64(h.DeploysTotal)
}

// MarshalJSON adds the derived failureRate field.
func (h ModelHealth) MarshalJSON() ([]byte, error) {
	type plain ModelHealth
	return json.Marshal(struct {
		plain
		FailureRate float64 `json:"failureRate"`
	}{plain(h), h.FailureRate()})
}

// HealthData is the payload of inference_health.changed events.
type HealthData struct {
	Status            string `json:"status"` // healthy | degraded | unavailable
	ReadyReplicas     int32  `json:"readyReplicas"`
	DesiredReplicas   int32  `json:"desiredReplicas"`
	AvailableReplicas int32  `json:"availableReplicas"`
}

// Project folds one event into a model's health row. It is pure and
// deterministic; idempotency (dedup by event ID) is the caller's job.
// It reports whether the event was relevant to the dashboard.
func Project(h *ModelHealth, ev Event) bool {
	if ev.Model == "" {
		return false
	}
	h.Model = ev.Model
	at := ev.OccurredAt
	switch ev.Type {
	case TypeDeployRequested:
		h.LastDeployVersion, h.LastDeployEnv, h.LastDeployStatus, h.LastDeployAt = ev.Version, ev.Environment, "pending", &at
	case TypeDeployReady:
		h.DeploysTotal++
		h.setStatusIfLatest(ev, "ready")
	case TypeDeployFailed:
		h.DeploysTotal++
		h.DeploysFailed++
		h.setStatusIfLatest(ev, "failed")
	case TypePromoteStarted:
		h.PromotionsTotal++
		h.LastDeployVersion, h.LastDeployEnv, h.LastDeployStatus, h.LastDeployAt = ev.Version, ev.Environment, "pending", &at
	case TypeRollbackStarted:
		h.RollbacksTotal++
		h.LastDeployVersion, h.LastDeployEnv, h.LastDeployStatus, h.LastDeployAt = ev.Version, ev.Environment, "pending", &at
	case TypeInferenceHealth:
		var d HealthData
		if err := json.Unmarshal(ev.Data, &d); err == nil {
			h.LastHealth = d.Status
			h.LastHealthAt = &at
		}
	default:
		return false
	}
	h.UpdatedAt = at
	return true
}

func (h *ModelHealth) setStatusIfLatest(ev Event, status string) {
	if h.LastDeployVersion == ev.Version && h.LastDeployEnv == ev.Environment {
		h.LastDeployStatus = status
	}
}
