// Package kube is ModelDock's boundary with Kubernetes. Client is the narrow
// interface the rest of the system uses; it has a real client-go
// implementation (NewClientGo) and an in-memory Fake used by tests and by
// `modeldock serve --kube=fake`. Reconciler drives in-flight deployments to
// ready/failed through that interface.
package kube

import (
	"context"
	"fmt"

	"github.com/wenxuewu0216/modeldock/domain"
)

// Labels and annotations ModelDock puts on every object it manages.
const (
	LabelManagedBy     = "app.kubernetes.io/managed-by"
	LabelName          = "app.kubernetes.io/name"
	LabelVersion       = "app.kubernetes.io/version"
	LabelEnvironment   = "modeldock.io/environment"
	ManagedByValue     = "modeldock"
	AnnotDeploymentID  = "modeldock.io/deployment-id"
	AnnotRevision      = "modeldock.io/revision"
	AnnotMetadataPrefx = "modeldock.io/meta-"
	FieldManager       = "modeldock"
)

// WorkloadSpec is everything needed to render the Deployment, Service and
// HorizontalPodAutoscaler for one model version in one environment.
type WorkloadSpec struct {
	Namespace    string
	Name         string // model name; also the object name
	Environment  domain.Environment
	Version      string
	Image        string
	Port         int32
	Resources    domain.Resources
	Scaling      domain.Scaling
	Metadata     map[string]string
	DeploymentID string
	Revision     int
	// ProgressDeadlineSeconds is set on the Deployment so Kubernetes itself
	// reports ProgressDeadlineExceeded; the reconciler also enforces it.
	ProgressDeadlineSeconds int32
}

// SpecFor builds the workload spec for deploying version v as deployment d.
func SpecFor(d domain.Deployment, v domain.ModelVersion, deadlineSeconds int32) WorkloadSpec {
	return WorkloadSpec{
		Namespace:               d.Namespace,
		Name:                    d.ModelName,
		Environment:             d.Environment,
		Version:                 v.Version,
		Image:                   v.Image,
		Port:                    v.Port,
		Resources:               v.Resources,
		Scaling:                 v.Scaling,
		Metadata:                v.Metadata,
		DeploymentID:            d.ID,
		Revision:                d.Revision,
		ProgressDeadlineSeconds: deadlineSeconds,
	}
}

// WorkloadStatus is the observed rollout state of a model's Deployment.
type WorkloadStatus struct {
	Exists bool
	// DeploymentID is the ModelDock deployment the live object was applied
	// for (from its annotation). Readiness only counts if it matches.
	DeploymentID string
	Generation   int64
	Observed     int64
	Desired      int32
	Updated      int32
	Ready        int32
	Available    int32
	Total        int32
	// Failed is set when Kubernetes reports the rollout cannot progress
	// (ProgressDeadlineExceeded) or the replica set cannot be created.
	Failed  bool
	Message string
}

// RolloutComplete mirrors `kubectl rollout status`: the controller has seen
// the latest spec, every replica runs the new template and is available,
// and no old replicas remain.
func (s WorkloadStatus) RolloutComplete(deploymentID string) bool {
	return s.Exists && s.DeploymentID == deploymentID &&
		s.Observed >= s.Generation &&
		s.Desired > 0 &&
		s.Updated == s.Desired && s.Available >= s.Desired && s.Total == s.Updated
}

// Health classifies a live workload for inference-health events.
func (s WorkloadStatus) Health() string {
	switch {
	case !s.Exists || s.Available == 0:
		return "unavailable"
	case s.Available < s.Desired:
		return "degraded"
	default:
		return "healthy"
	}
}

// Client is the Kubernetes operations ModelDock needs.
type Client interface {
	// Apply creates or updates the Deployment, Service and HPA for spec.
	// It must be idempotent: the reconciler re-applies after crashes.
	Apply(ctx context.Context, spec WorkloadSpec) error
	// Status returns the observed state of namespace/name.
	Status(ctx context.Context, namespace, name string) (WorkloadStatus, error)
	// Delete removes all objects for namespace/name.
	Delete(ctx context.Context, namespace, name string) error
}

// ValidateResources checks the resource quantities parse as Kubernetes
// quantities and that limits are not below requests.
func ValidateResources(r domain.Resources) error {
	_, err := buildResources(r)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrInvalid, err)
	}
	return nil
}
