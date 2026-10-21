package events

import (
	"encoding/json"
	"testing"
	"time"
)

func TestTopicRouting(t *testing.T) {
	cases := map[string]string{
		TypeModelRegistered:    TopicRegistry,
		TypeDeployRequested:    TopicDeploy,
		TypeDeployFailed:       TopicDeploy,
		TypePromoteStarted:     TopicPromote,
		TypeRollbackStarted:    TopicRollback,
		TypePromotionRequested: TopicApproval,
		TypeApprovalApproved:   TopicApproval,
		TypeInferenceHealth:    TopicInferenceHealth,
		"unknown.thing":        AuditTopic,
	}
	for typ, want := range cases {
		if got := (Event{Type: typ}).Topic(); got != want {
			t.Errorf("%s -> %s, want %s", typ, got, want)
		}
	}
}

func TestExpandFansOutToAudit(t *testing.T) {
	r := &Relay{}
	out := r.Expand([]OutboxMessage{{EventID: "e1", Topic: TopicDeploy, Key: "iris", Payload: []byte("{}")}})
	if len(out) != 2 || out[0].Topic != TopicDeploy || out[1].Topic != AuditTopic || out[1].Key != "iris" {
		t.Fatalf("expand = %+v", out)
	}
	r.AuditTopic = "-"
	if len(r.Expand([]OutboxMessage{{Topic: TopicDeploy}})) != 1 {
		t.Fatal("audit fan-out not disabled")
	}
}

func TestProjectDashboard(t *testing.T) {
	var h ModelHealth
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ev := func(typ, version, env string) Event {
		at = at.Add(time.Minute)
		return Event{ID: typ + version, Type: typ, Model: "iris", Version: version, Environment: env, OccurredAt: at}
	}
	seq := []Event{
		ev(TypeDeployRequested, "1", "staging"),
		ev(TypeDeployReady, "1", "staging"),
		ev(TypePromoteStarted, "1", "prod"),
		ev(TypeDeployReady, "1", "prod"),
		ev(TypeDeployRequested, "2", "staging"),
		ev(TypeDeployFailed, "2", "staging"),
		ev(TypeRollbackStarted, "1", "staging"),
		ev(TypeDeployReady, "1", "staging"),
	}
	for _, e := range seq {
		if !Project(&h, e) {
			t.Fatalf("event %s ignored", e.Type)
		}
	}
	health, _ := json.Marshal(HealthData{Status: "degraded"})
	Project(&h, Event{Type: TypeInferenceHealth, Model: "iris", Data: health, OccurredAt: at})
	if Project(&h, Event{Type: TypeVersionRegistered, Model: "iris"}) {
		t.Error("registry event should not affect dashboard")
	}
	if h.DeploysTotal != 4 || h.DeploysFailed != 1 || h.PromotionsTotal != 1 || h.RollbacksTotal != 1 {
		t.Fatalf("counts = %+v", h)
	}
	if h.FailureRate() != 0.25 {
		t.Fatalf("failure rate = %v", h.FailureRate())
	}
	if h.LastDeployVersion != "1" || h.LastDeployEnv != "staging" || h.LastDeployStatus != "ready" || h.LastHealth != "degraded" {
		t.Fatalf("last = %+v", h)
	}
	b, _ := json.Marshal(h)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["failureRate"] != 0.25 {
		t.Fatalf("json = %s", b)
	}
}
