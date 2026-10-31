package stream

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/wenxuewu0216/modeldock/events"
)

// Message is the JSON payload of one SSE event. It is the audit event
// envelope plus a few fields lifted out of Data so clients need not know
// each event type's payload shape.
type Message struct {
	// Seq is the audit log sequence number; it is also the SSE id.
	Seq          int64  `json:"seq"`
	EventID      string `json:"eventId"`
	Type         string `json:"type"`
	Actor        string `json:"actor"`
	Model        string `json:"model,omitempty"`
	Version      string `json:"version,omitempty"`
	Environment  string `json:"environment,omitempty"`
	DeploymentID string `json:"deploymentId,omitempty"`
	// Revision, Kind and Status are set for deployment events.
	Revision int    `json:"revision,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Status   string `json:"status,omitempty"`
	// ApprovalID is set for approval events.
	ApprovalID string          `json:"approvalId,omitempty"`
	OccurredAt time.Time       `json:"occurredAt"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// deploymentStatus maps event types to the deployment status they record.
var deploymentStatus = map[string]string{
	events.TypeDeployRequested:   "pending",
	events.TypePromoteStarted:    "pending",
	events.TypeRollbackStarted:   "pending",
	events.TypeDeployProgressing: "progressing",
	events.TypeDeployReady:       "ready",
	events.TypeDeployFailed:      "failed",
}

// FromEvent builds the stream message for ev.
func FromEvent(ev events.Event) Message {
	m := Message{
		Seq: ev.Seq, EventID: ev.ID, Type: ev.Type, Actor: ev.Actor, Model: ev.Model, Version: ev.Version,
		Environment: ev.Environment, DeploymentID: ev.DeploymentID, OccurredAt: ev.OccurredAt, Data: ev.Data,
		Status: deploymentStatus[ev.Type],
	}
	var d struct {
		Revision   int    `json:"revision"`
		Kind       string `json:"kind"`
		ApprovalID string `json:"approvalId"`
	}
	if len(ev.Data) > 0 {
		_ = json.Unmarshal(ev.Data, &d)
	}
	m.Revision, m.Kind, m.ApprovalID = d.Revision, d.Kind, d.ApprovalID
	switch ev.Type {
	case events.TypeDeployRequested:
		m.Kind = "deploy"
	case events.TypePromoteStarted:
		m.Kind = "promote"
	}
	return m
}

// Encode renders ev in SSE wire format: id, event name and one data line.
func Encode(ev events.Event) ([]byte, error) {
	b, err := json.Marshal(FromEvent(ev))
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(b)+len(ev.Type)+32)
	out = append(out, "id: "...)
	out = strconv.AppendInt(out, ev.Seq, 10)
	out = append(out, "\nevent: "...)
	out = append(out, ev.Type...)
	out = append(out, "\ndata: "...)
	out = append(out, b...)
	out = append(out, "\n\n"...)
	return out, nil
}
