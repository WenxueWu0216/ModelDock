// Package events defines ModelDock's audit/domain event envelope, the outbox
// relay that publishes events to Kafka, and the consumer that builds the
// health dashboard read model.
package events

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Event types. The prefix before the first dot selects the Kafka topic.
const (
	TypeModelRegistered    = "registry.model_registered"
	TypeVersionRegistered  = "registry.version_registered"
	TypeDeployRequested    = "deploy.requested"
	TypeDeployReady        = "deploy.ready"
	TypeDeployFailed       = "deploy.failed"
	TypePromotionRequested = "approval.requested"
	TypeApprovalApproved   = "approval.approved"
	TypeApprovalRejected   = "approval.rejected"
	TypePromoteStarted     = "promote.started"
	TypeRollbackStarted    = "rollback.started"
	TypeInferenceHealth    = "inference_health.changed"
)

// Topics. Every event goes to its domain topic and to AuditTopic, which is
// the immutable, long-retention copy of the audit table.
const (
	TopicRegistry        = "modeldock.registry"
	TopicDeploy          = "modeldock.deploy"
	TopicPromote         = "modeldock.promote"
	TopicRollback        = "modeldock.rollback"
	TopicApproval        = "modeldock.approval"
	TopicInferenceHealth = "modeldock.inference-health"
	AuditTopic           = "modeldock.audit"
)

// AllTopics lists every topic ModelDock writes, for provisioning.
var AllTopics = []string{TopicRegistry, TopicDeploy, TopicPromote, TopicRollback,
	TopicApproval, TopicInferenceHealth, AuditTopic}

// Event is the envelope persisted to audit_events and published to Kafka.
// ID is globally unique and is what consumers deduplicate on, because the
// outbox relay delivers at least once.
type Event struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Actor        string          `json:"actor"`
	Model        string          `json:"model,omitempty"`
	Version      string          `json:"version,omitempty"`
	Environment  string          `json:"environment,omitempty"`
	DeploymentID string          `json:"deploymentId,omitempty"`
	OccurredAt   time.Time       `json:"occurredAt"`
	Data         json.RawMessage `json:"data,omitempty"`
	// Seq is the audit log sequence number; zero until persisted.
	Seq int64 `json:"seq,omitempty"`
}

// New builds an event with a fresh ID and timestamp. data is marshalled to
// JSON; a marshal failure is a programming error and panics.
func New(typ, actor string, data any) Event {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			panic("events: marshal event data: " + err.Error())
		}
		raw = b
	}
	return Event{ID: uuid.NewString(), Type: typ, Actor: actor, OccurredAt: time.Now().UTC(), Data: raw}
}

// Topic returns the domain topic for the event type.
func (e Event) Topic() string {
	prefix, _, _ := strings.Cut(e.Type, ".")
	switch prefix {
	case "registry":
		return TopicRegistry
	case "deploy":
		return TopicDeploy
	case "promote":
		return TopicPromote
	case "rollback":
		return TopicRollback
	case "approval":
		return TopicApproval
	case "inference_health":
		return TopicInferenceHealth
	}
	return AuditTopic
}

// Key is the Kafka partition key: all events of one model land in one
// partition, so they are consumed in the order they were committed.
func (e Event) Key() string {
	if e.Model != "" {
		return e.Model
	}
	return e.ID
}
