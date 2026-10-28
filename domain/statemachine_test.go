package domain

import (
	"errors"
	"testing"
)

var allEvents = []VersionEvent{
	EvDeployStaging, EvRequestPromotion, EvApprove, EvReject, EvPromote,
	EvSupersede, EvRollbackFrom, EvRollbackToProd, EvRollbackToStaging, EvFail,
}

func apply(t *testing.T, s VersionState, evs ...VersionEvent) VersionState {
	t.Helper()
	for _, ev := range evs {
		next, err := Transition(s, ev)
		if err != nil {
			t.Fatalf("Transition(%s, %s): %v", s, ev, err)
		}
		s = next
	}
	return s
}

func TestHappyPathToProduction(t *testing.T) {
	got := apply(t, StateRegistered, EvDeployStaging, EvRequestPromotion, EvApprove, EvPromote)
	if got != StateProduction {
		t.Fatalf("got %s, want production", got)
	}
}

func TestRollbackLifecycle(t *testing.T) {
	// v1 live in prod, v2 promoted over it, then prod rolled back to v1.
	v1 := apply(t, StateRegistered, EvDeployStaging, EvRequestPromotion, EvApprove, EvPromote)
	v2 := apply(t, StateRegistered, EvDeployStaging, EvRequestPromotion, EvApprove, EvPromote)
	v1 = apply(t, v1, EvSupersede)
	if v1 != StateSuperseded {
		t.Fatalf("v1 = %s, want superseded", v1)
	}
	v2 = apply(t, v2, EvRollbackFrom)
	v1 = apply(t, v1, EvRollbackToProd)
	if v1 != StateProduction || v2 != StateRolledBack {
		t.Fatalf("after rollback v1=%s v2=%s", v1, v2)
	}
	// The rolled-back version can be fixed forward through staging again.
	v2 = apply(t, v2, EvDeployStaging)
	if v2 != StateStaging {
		t.Fatalf("v2 = %s, want staging", v2)
	}
}

func TestRejectReturnsToStaging(t *testing.T) {
	got := apply(t, StateRegistered, EvDeployStaging, EvRequestPromotion, EvReject)
	if got != StateStaging {
		t.Fatalf("got %s", got)
	}
	// and may be re-requested
	if apply(t, got, EvRequestPromotion) != StatePendingApproval {
		t.Fatal("re-request after reject failed")
	}
}

func TestApprovalGateCannotBeSkipped(t *testing.T) {
	// Every state except approved must refuse promote: there is no path to
	// production that bypasses the approval decision.
	for _, s := range AllVersionStates {
		_, err := Transition(s, EvPromote)
		if s == StateApproved {
			if err != nil {
				t.Errorf("approved should promote: %v", err)
			}
			continue
		}
		if !errors.Is(err, ErrConflict) {
			t.Errorf("promote from %s: err = %v, want ErrConflict", s, err)
		}
	}
	// A never-live version cannot be "rolled back to" in prod either.
	for _, s := range []VersionState{StateRegistered, StateStaging, StatePendingApproval, StateApproved, StateFailed} {
		if CanTransition(s, EvRollbackToProd) {
			t.Errorf("rollback_to_prod allowed from %s, which never passed prod", s)
		}
	}
}

func TestOnlyApprovalOrRollbackReachProduction(t *testing.T) {
	for _, s := range AllVersionStates {
		for _, ev := range allEvents {
			to, err := Transition(s, ev)
			if err != nil || to != StateProduction {
				continue
			}
			ok := (s == StateApproved && ev == EvPromote) ||
				((s == StateSuperseded || s == StateRolledBack) && ev == EvRollbackToProd)
			if !ok {
				t.Errorf("unexpected path to production: %s --%s-->", s, ev)
			}
		}
	}
}

func TestTransitionTableIsClosed(t *testing.T) {
	known := map[VersionState]bool{}
	for _, s := range AllVersionStates {
		known[s] = true
	}
	for k, to := range transitions {
		if !known[k.from] || !known[to] {
			t.Errorf("transition %v -> %s references unknown state", k, to)
		}
	}
	// Every state must be reachable from registered.
	reach := map[VersionState]bool{StateRegistered: true}
	for changed := true; changed; {
		changed = false
		for k, to := range transitions {
			if reach[k.from] && !reach[to] {
				reach[to] = true
				changed = true
			}
		}
	}
	for _, s := range AllVersionStates {
		if !reach[s] {
			t.Errorf("state %s unreachable", s)
		}
	}
}

func TestInvalidTransitionLeavesStateUnchanged(t *testing.T) {
	got, err := Transition(StateRegistered, EvApprove)
	if err == nil || got != StateRegistered {
		t.Fatalf("got %s, %v", got, err)
	}
}

func TestDiff(t *testing.T) {
	a := ModelVersion{ModelName: "iris", Version: "1", Image: "img:1", Port: 8080,
		Resources: Resources{CPURequest: "100m", MemoryRequest: "128Mi"},
		Scaling:   Scaling{MinReplicas: 1, MaxReplicas: 3, TargetCPUPercentage: 70},
		Metadata:  map[string]string{"framework": "sklearn", "accuracy": "0.95"}}
	b := a
	b.Version = "2"
	b.Image = "img:2"
	b.Resources.CPURequest = "200m"
	b.Metadata = map[string]string{"framework": "sklearn", "accuracy": "0.97", "dataset": "iris-v2"}

	d := Diff(a, b)
	want := []FieldChange{
		{"image", "img:1", "img:2"},
		{"resources.cpuRequest", "100m", "200m"},
		{"metadata.accuracy", "0.95", "0.97"},
		{"metadata.dataset", "", "iris-v2"},
	}
	if len(d.Changes) != len(want) {
		t.Fatalf("changes = %+v", d.Changes)
	}
	for i := range want {
		if d.Changes[i] != want[i] {
			t.Errorf("change[%d] = %+v, want %+v", i, d.Changes[i], want[i])
		}
	}
	if len(Diff(a, a).Changes) != 0 {
		t.Error("self-diff not empty")
	}
}

func TestValidateDefaults(t *testing.T) {
	v := ModelVersion{Version: "1.0.0", Image: "ghcr.io/acme/iris:1.0.0"}
	if err := v.Validate(); err != nil {
		t.Fatal(err)
	}
	if v.Port != 8080 || v.Scaling.MinReplicas != 1 || v.Scaling.MaxReplicas != 3 || v.State != StateRegistered {
		t.Fatalf("defaults not applied: %+v", v)
	}
	bad := []ModelVersion{
		{Version: "", Image: "x"},
		{Version: "1.0", Image: "UPPER/case"},
		{Version: "1.0", Image: "x", Scaling: Scaling{MinReplicas: 3, MaxReplicas: 2}},
		{Version: "has space", Image: "x"},
	}
	for _, b := range bad {
		if err := b.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("Validate(%+v) = %v, want ErrInvalid", b, err)
		}
	}
	if err := ValidateModelName("Iris_Model"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad model name accepted")
	}
}
