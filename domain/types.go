// Package domain holds ModelDock's core entities and the pure rules that govern
// them (version lifecycle state machine, validation, version diffing). It has
// no I/O dependencies so it can be unit-tested exhaustively.
package domain

import (
	"fmt"
	"regexp"
	"time"
)

// Environment is a deployment target. Each environment maps to one
// Kubernetes namespace.
type Environment string

const (
	EnvStaging Environment = "staging"
	EnvProd    Environment = "prod"
)

// Valid reports whether e is a known environment.
func (e Environment) Valid() bool { return e == EnvStaging || e == EnvProd }

// ParseEnvironment converts user input into an Environment.
func ParseEnvironment(s string) (Environment, error) {
	e := Environment(s)
	if !e.Valid() {
		return "", fmt.Errorf("%w: unknown environment %q (want staging or prod)", ErrInvalid, s)
	}
	return e, nil
}

// Role is an RBAC role carried in the JWT "role" claim.
type Role string

const (
	RoleViewer   Role = "viewer"
	RoleDeployer Role = "deployer"
	RoleApprover Role = "approver"
	RoleAdmin    Role = "admin"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleViewer, RoleDeployer, RoleApprover, RoleAdmin:
		return true
	}
	return false
}

// User is an authenticated principal. Identity is owned by the token issuer;
// ModelDock does not store users, it records their subject on every action.
type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role Role   `json:"role"`
}

// Model is a named, deployable ML model. Its name doubles as the Kubernetes
// workload name, so it must be a DNS-1123 label.
type Model struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedBy   string    `json:"createdBy"`
	CreatedAt   time.Time `json:"createdAt"`
}

// Resources are the container resource requests/limits for a model server.
type Resources struct {
	CPURequest    string `json:"cpuRequest"`
	MemoryRequest string `json:"memoryRequest"`
	CPULimit      string `json:"cpuLimit,omitempty"`
	MemoryLimit   string `json:"memoryLimit,omitempty"`
}

// Scaling controls the HorizontalPodAutoscaler created for a version.
type Scaling struct {
	MinReplicas         int32 `json:"minReplicas"`
	MaxReplicas         int32 `json:"maxReplicas"`
	TargetCPUPercentage int32 `json:"targetCPUPercentage"`
}

// ModelVersion is an immutable, registered build of a model: a container
// image plus the runtime configuration needed to serve it. Only State changes
// after registration.
type ModelVersion struct {
	ID        string            `json:"id"`
	ModelID   string            `json:"modelId"`
	ModelName string            `json:"modelName"`
	Version   string            `json:"version"`
	Image     string            `json:"image"`
	Port      int32             `json:"port"`
	Metadata  map[string]string `json:"metadata"`
	Resources Resources         `json:"resources"`
	Scaling   Scaling           `json:"scaling"`
	State     VersionState      `json:"state"`
	CreatedBy string            `json:"createdBy"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

// DeploymentKind records why a deployment revision was created.
type DeploymentKind string

const (
	KindDeploy       DeploymentKind = "deploy"
	KindPromote      DeploymentKind = "promote"
	KindRollback     DeploymentKind = "rollback"
	KindAutoRollback DeploymentKind = "auto_rollback"
)

// DeploymentStatus is the runtime status of one deployment revision.
type DeploymentStatus string

const (
	// DeployPending: recorded in the DB, not yet applied to Kubernetes.
	DeployPending DeploymentStatus = "pending"
	// DeployProgressing: applied, waiting for the rollout to become ready.
	DeployProgressing DeploymentStatus = "progressing"
	// DeployReady: all replicas of this revision are available.
	DeployReady DeploymentStatus = "ready"
	// DeployFailed: apply failed or the rollout missed its readiness deadline.
	DeployFailed DeploymentStatus = "failed"
	// DeploySuperseded: a newer revision replaced this one normally.
	DeploySuperseded DeploymentStatus = "superseded"
	// DeployRolledBack: replaced by a rollback to an older version.
	DeployRolledBack DeploymentStatus = "rolled_back"
)

// InFlight reports whether the reconciler still owns this revision.
func (s DeploymentStatus) InFlight() bool { return s == DeployPending || s == DeployProgressing }

// Deployment is one revision of a model running in an environment. The
// history of a (model, environment) pair is its ordered list of revisions;
// the highest revision is the desired state.
type Deployment struct {
	ID          string           `json:"id"`
	ModelID     string           `json:"modelId"`
	ModelName   string           `json:"modelName"`
	VersionID   string           `json:"versionId"`
	Version     string           `json:"version"`
	Environment Environment      `json:"environment"`
	Namespace   string           `json:"namespace"`
	Revision    int              `json:"revision"`
	Kind        DeploymentKind   `json:"kind"`
	Status      DeploymentStatus `json:"status"`
	Reason      string           `json:"reason,omitempty"`
	CreatedBy   string           `json:"createdBy"`
	CreatedAt   time.Time        `json:"createdAt"`
	UpdatedAt   time.Time        `json:"updatedAt"`
	StartedAt   *time.Time       `json:"startedAt,omitempty"`
	ReadyAt     *time.Time       `json:"readyAt,omitempty"`
	FinishedAt  *time.Time       `json:"finishedAt,omitempty"`
}

// ApprovalStatus is the decision state of a promotion request.
type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalRejected ApprovalStatus = "rejected"
)

// Approval is a request to promote a version to prod and its decision.
type Approval struct {
	ID          string         `json:"id"`
	VersionID   string         `json:"versionId"`
	ModelName   string         `json:"modelName"`
	Version     string         `json:"version"`
	Status      ApprovalStatus `json:"status"`
	RequestedBy string         `json:"requestedBy"`
	RequestedAt time.Time      `json:"requestedAt"`
	DecidedBy   string         `json:"decidedBy,omitempty"`
	DecidedAt   *time.Time     `json:"decidedAt,omitempty"`
	Comment     string         `json:"comment,omitempty"`
}

var (
	dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	versionRe    = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)
	// image references: [registry/]repo[:tag][@digest]; intentionally loose.
	imageRe = regexp.MustCompile(`^[a-z0-9]+([._/:-][a-zA-Z0-9]+)*(@sha256:[a-f0-9]{64})?$`)
)

// ValidateModelName checks the name is usable as a Kubernetes object name.
// 50 chars leaves room for suffixes added to generated objects.
func ValidateModelName(name string) error {
	if len(name) == 0 || len(name) > 50 || !dns1123Label.MatchString(name) {
		return fmt.Errorf("%w: model name %q must be a DNS-1123 label of at most 50 chars", ErrInvalid, name)
	}
	return nil
}

// Validate checks a version is well-formed before registration and fills in
// defaults for omitted fields.
func (v *ModelVersion) Validate() error {
	if !versionRe.MatchString(v.Version) {
		return fmt.Errorf("%w: version %q must match %s (it is used as a label value)", ErrInvalid, v.Version, versionRe)
	}
	if !imageRe.MatchString(v.Image) {
		return fmt.Errorf("%w: image %q is not a valid image reference", ErrInvalid, v.Image)
	}
	if v.Port == 0 {
		v.Port = 8080
	}
	if v.Port < 1 || v.Port > 65535 {
		return fmt.Errorf("%w: port %d out of range", ErrInvalid, v.Port)
	}
	if v.Resources.CPURequest == "" {
		v.Resources.CPURequest = "100m"
	}
	if v.Resources.MemoryRequest == "" {
		v.Resources.MemoryRequest = "128Mi"
	}
	if v.Scaling.MinReplicas == 0 {
		v.Scaling.MinReplicas = 1
	}
	if v.Scaling.MaxReplicas == 0 {
		v.Scaling.MaxReplicas = v.Scaling.MinReplicas * 3
	}
	if v.Scaling.TargetCPUPercentage == 0 {
		v.Scaling.TargetCPUPercentage = 70
	}
	if v.Scaling.MinReplicas < 1 || v.Scaling.MaxReplicas < v.Scaling.MinReplicas {
		return fmt.Errorf("%w: scaling requires 1 <= minReplicas <= maxReplicas (got %d..%d)",
			ErrInvalid, v.Scaling.MinReplicas, v.Scaling.MaxReplicas)
	}
	if v.Scaling.TargetCPUPercentage < 1 || v.Scaling.TargetCPUPercentage > 100 {
		return fmt.Errorf("%w: targetCPUPercentage must be in 1..100", ErrInvalid)
	}
	if v.Metadata == nil {
		v.Metadata = map[string]string{}
	}
	if v.State == "" {
		v.State = StateRegistered
	}
	return nil
}
