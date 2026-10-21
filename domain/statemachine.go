package domain

import "fmt"

// VersionState is the promotion lifecycle state of a ModelVersion.
//
//	registered ──deploy──▶ staging ──request──▶ pending_approval ──approve──▶ approved ──promote──▶ production
//	                │  ▲                              │                                               │
//	        fail    ▼  │ redeploy           reject    ▼                                    supersede /│ rollback-from
//	             failed ◀───── staging ◀──────────────┘                                  superseded / rolled_back
//	                                                                                              │
//	                                          rollback-to (prod) ◀────────────────────────────────┘
//
// The table in transitions is the single source of truth; the diagram is a
// summary.
type VersionState string

const (
	StateRegistered      VersionState = "registered"
	StateStaging         VersionState = "staging"
	StatePendingApproval VersionState = "pending_approval"
	StateApproved        VersionState = "approved"
	StateProduction      VersionState = "production"
	StateSuperseded      VersionState = "superseded"
	StateRolledBack      VersionState = "rolled_back"
	StateFailed          VersionState = "failed"
)

// AllVersionStates lists every state, for validation and exhaustive tests.
var AllVersionStates = []VersionState{
	StateRegistered, StateStaging, StatePendingApproval, StateApproved,
	StateProduction, StateSuperseded, StateRolledBack, StateFailed,
}

// VersionEvent is an input to the version state machine.
type VersionEvent string

const (
	// EvDeployStaging: the version is (re)deployed to staging.
	EvDeployStaging VersionEvent = "deploy_staging"
	// EvRequestPromotion: a deployer asks for prod promotion.
	EvRequestPromotion VersionEvent = "request_promotion"
	// EvApprove / EvReject: an approver decides on the request.
	EvApprove VersionEvent = "approve"
	EvReject  VersionEvent = "reject"
	// EvPromote: the approved version is rolled out to prod.
	EvPromote VersionEvent = "promote"
	// EvSupersede: a newer version was promoted over this one.
	EvSupersede VersionEvent = "supersede"
	// EvRollbackFrom: this version was the live one and was rolled back.
	EvRollbackFrom VersionEvent = "rollback_from"
	// EvRollbackToProd: this previously-live version is live in prod again.
	EvRollbackToProd VersionEvent = "rollback_to_prod"
	// EvRollbackToStaging: this version is live in staging again.
	EvRollbackToStaging VersionEvent = "rollback_to_staging"
	// EvFail: the version's rollout did not become ready in time.
	EvFail VersionEvent = "fail"
)

type transitionKey struct {
	from VersionState
	ev   VersionEvent
}

var transitions = map[transitionKey]VersionState{
	{StateRegistered, EvDeployStaging}: StateStaging,
	{StateStaging, EvDeployStaging}:    StateStaging,
	{StateFailed, EvDeployStaging}:     StateStaging,
	{StateRolledBack, EvDeployStaging}: StateStaging,
	{StateSuperseded, EvDeployStaging}: StateStaging,

	{StateStaging, EvRequestPromotion}: StatePendingApproval,

	{StatePendingApproval, EvApprove}: StateApproved,
	{StatePendingApproval, EvReject}:  StateStaging,

	{StateApproved, EvPromote}: StateProduction,

	{StateProduction, EvSupersede}: StateSuperseded,

	{StateProduction, EvRollbackFrom}: StateRolledBack,
	{StateStaging, EvRollbackFrom}:    StateRolledBack,

	// Only versions that were live in prod before may be rolled back to in
	// prod: they passed an approval gate at that time.
	{StateSuperseded, EvRollbackToProd}: StateProduction,
	{StateRolledBack, EvRollbackToProd}: StateProduction,

	{StateRolledBack, EvRollbackToStaging}: StateStaging,
	{StateSuperseded, EvRollbackToStaging}: StateStaging,
	{StateFailed, EvRollbackToStaging}:     StateStaging,
	{StateStaging, EvRollbackToStaging}:    StateStaging,

	{StateStaging, EvFail}:    StateFailed,
	{StateProduction, EvFail}: StateFailed,
}

// Transition returns the state reached by applying ev in state from, or an
// ErrConflict-wrapped error if the transition is not allowed.
func Transition(from VersionState, ev VersionEvent) (VersionState, error) {
	to, ok := transitions[transitionKey{from, ev}]
	if !ok {
		return from, fmt.Errorf("%w: cannot %s a version in state %s", ErrConflict, ev, from)
	}
	return to, nil
}

// CanTransition reports whether ev is allowed in state from.
func CanTransition(from VersionState, ev VersionEvent) bool {
	_, ok := transitions[transitionKey{from, ev}]
	return ok
}
