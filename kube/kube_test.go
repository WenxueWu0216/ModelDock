package kube

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/wenxuewu0216/modeldock/domain"
)

func testSpec() WorkloadSpec {
	return WorkloadSpec{
		Namespace: "modeldock-staging", Name: "iris", Environment: domain.EnvStaging,
		Version: "1.0.0", Image: "ghcr.io/acme/iris:1.0.0", Port: 8080,
		Resources:    domain.Resources{CPURequest: "100m", MemoryRequest: "128Mi", CPULimit: "500m", MemoryLimit: "256Mi"},
		Scaling:      domain.Scaling{MinReplicas: 2, MaxReplicas: 5, TargetCPUPercentage: 60},
		Metadata:     map[string]string{"framework": "sklearn", "bad key!": "dropped"},
		DeploymentID: "dep-1", Revision: 3, ProgressDeadlineSeconds: 120,
	}
}

func TestBuildManifests(t *testing.T) {
	objs, err := Build(testSpec(), 0)
	if err != nil {
		t.Fatal(err)
	}
	d := objs.Deployment
	if *d.Spec.Replicas != 2 {
		t.Errorf("replicas = %d, want minReplicas 2", *d.Spec.Replicas)
	}
	if *d.Spec.ProgressDeadlineSeconds != 120 {
		t.Errorf("progress deadline = %d", *d.Spec.ProgressDeadlineSeconds)
	}
	if d.Annotations[AnnotDeploymentID] != "dep-1" || d.Labels[LabelManagedBy] != ManagedByValue {
		t.Errorf("metadata = %v %v", d.Annotations, d.Labels)
	}
	// Selector must not include the version label, or rolling updates
	// between versions would orphan pods.
	if _, ok := d.Spec.Selector.MatchLabels[LabelVersion]; ok {
		t.Error("selector includes version label")
	}
	for k, v := range d.Spec.Selector.MatchLabels {
		if d.Spec.Template.Labels[k] != v {
			t.Errorf("template labels do not match selector %s=%s", k, v)
		}
	}
	c := d.Spec.Template.Spec.Containers[0]
	if c.Image != "ghcr.io/acme/iris:1.0.0" || c.ReadinessProbe.HTTPGet.Path != "/health" {
		t.Errorf("container = %+v", c)
	}
	if c.Resources.Limits.Cpu().String() != "500m" || c.Resources.Requests.Memory().String() != "128Mi" {
		t.Errorf("resources = %+v", c.Resources)
	}
	if _, ok := d.Spec.Template.Annotations[AnnotMetadataPrefx+"framework"]; !ok {
		t.Error("safe metadata key not propagated")
	}
	for k := range d.Spec.Template.Annotations {
		if strings.Contains(k, "bad key") {
			t.Error("unsafe metadata key propagated")
		}
	}
	if objs.HPA.Spec.MaxReplicas != 5 || *objs.HPA.Spec.MinReplicas != 2 || objs.HPA.Spec.ScaleTargetRef.Name != "iris" {
		t.Errorf("hpa = %+v", objs.HPA.Spec)
	}
	if objs.Service.Spec.Ports[0].TargetPort.StrVal != "http" {
		t.Errorf("service = %+v", objs.Service.Spec)
	}
	// Live replica count (scaled by HPA) is preserved but clamped.
	if o, _ := Build(testSpec(), 4); *o.Deployment.Spec.Replicas != 4 {
		t.Error("live replicas not preserved")
	}
	if o, _ := Build(testSpec(), 50); *o.Deployment.Spec.Replicas != 5 {
		t.Error("replicas not clamped to max")
	}
}

func TestValidateResources(t *testing.T) {
	if err := ValidateResources(domain.Resources{CPURequest: "lots"}); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("bad quantity: %v", err)
	}
	if err := ValidateResources(domain.Resources{CPURequest: "1", CPULimit: "500m"}); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("limit below request: %v", err)
	}
	if err := ValidateResources(domain.Resources{CPURequest: "250m", MemoryRequest: "1Gi"}); err != nil {
		t.Error(err)
	}
}

func TestRenderYAML(t *testing.T) {
	b, err := RenderYAML(testSpec())
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"kind: Deployment", "kind: Service", "kind: HorizontalPodAutoscaler", "apiVersion: autoscaling/v2"} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered YAML missing %q", want)
		}
	}
	if strings.Count(s, "\n---\n") != 2 {
		t.Error("expected 3 YAML documents")
	}
}

func TestStatusFromDeployment(t *testing.T) {
	three := int32(3)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Generation: 4, Annotations: map[string]string{AnnotDeploymentID: "dep-9"}},
		Spec:       appsv1.DeploymentSpec{Replicas: &three},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 4, Replicas: 3, UpdatedReplicas: 3,
			ReadyReplicas: 3, AvailableReplicas: 3},
	}
	st := StatusFromDeployment(d)
	if !st.RolloutComplete("dep-9") {
		t.Fatalf("expected complete: %+v", st)
	}
	if st.RolloutComplete("dep-other") {
		t.Fatal("complete for a different deployment ID")
	}
	// Old replicas still terminating -> not complete.
	d.Status.Replicas = 4
	if StatusFromDeployment(d).RolloutComplete("dep-9") {
		t.Fatal("complete with old replicas present")
	}
	d.Status.Replicas = 3
	// Controller has not observed the latest generation yet.
	d.Status.ObservedGeneration = 3
	if StatusFromDeployment(d).RolloutComplete("dep-9") {
		t.Fatal("complete before generation observed")
	}
	d.Status.Conditions = []appsv1.DeploymentCondition{{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded", Message: "timed out"}}
	if st := StatusFromDeployment(d); !st.Failed || !strings.Contains(st.Message, "ProgressDeadlineExceeded") {
		t.Fatalf("failure not detected: %+v", st)
	}
}

func TestHealthClassification(t *testing.T) {
	cases := []struct {
		st   WorkloadStatus
		want string
	}{
		{WorkloadStatus{}, "unavailable"},
		{WorkloadStatus{Exists: true, Desired: 3, Available: 0}, "unavailable"},
		{WorkloadStatus{Exists: true, Desired: 3, Available: 2}, "degraded"},
		{WorkloadStatus{Exists: true, Desired: 3, Available: 3}, "healthy"},
	}
	for _, c := range cases {
		if got := c.st.Health(); got != c.want {
			t.Errorf("%+v: %s, want %s", c.st, got, c.want)
		}
	}
}

// --- reconciler ---

type recorder struct {
	mu          sync.Mutex
	deployments map[string]*domain.Deployment
	version     domain.ModelVersion
	calls       []string
	health      []string
}

func (r *recorder) ListInFlight(context.Context) ([]domain.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Deployment
	for _, d := range r.deployments {
		if d.Status.InFlight() {
			out = append(out, *d)
		}
	}
	return out, nil
}

func (r *recorder) ListLive(context.Context) ([]domain.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Deployment
	for _, d := range r.deployments {
		if d.Status == domain.DeployReady {
			out = append(out, *d)
		}
	}
	return out, nil
}

func (r *recorder) GetVersionByID(context.Context, string) (domain.ModelVersion, error) {
	return r.version, nil
}

func (r *recorder) TryLeader(context.Context, int64) (func(), bool, error) { return func() {}, true, nil }

func (r *recorder) set(id string, s domain.DeploymentStatus, call string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deployments[id].Status = s
	if s == domain.DeployProgressing {
		now := time.Now()
		r.deployments[id].StartedAt = &now
	}
	r.calls = append(r.calls, call)
	return nil
}

func (r *recorder) MarkProgressing(_ context.Context, id string) error {
	return r.set(id, domain.DeployProgressing, "progressing")
}
func (r *recorder) MarkReady(_ context.Context, id string) error {
	return r.set(id, domain.DeployReady, "ready")
}
func (r *recorder) MarkFailed(_ context.Context, id, reason string) error {
	return r.set(id, domain.DeployFailed, "failed:"+reason)
}
func (r *recorder) ReportHealth(_ context.Context, _ domain.Deployment, h string, _ WorkloadStatus) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health = append(r.health, h)
	return nil
}

func newRecorder() *recorder {
	return &recorder{
		deployments: map[string]*domain.Deployment{"d1": {
			ID: "d1", ModelName: "iris", Namespace: "modeldock-staging", Environment: domain.EnvStaging,
			Status: domain.DeployPending, CreatedAt: time.Now(), Revision: 1,
		}},
		version: domain.ModelVersion{Version: "1", Image: "img:1", Port: 8080,
			Resources: domain.Resources{CPURequest: "100m", MemoryRequest: "64Mi"},
			Scaling:   domain.Scaling{MinReplicas: 2, MaxReplicas: 3, TargetCPUPercentage: 70}},
	}
}

func TestReconcilerHappyPath(t *testing.T) {
	rec, fake := newRecorder(), NewFake()
	fake.ReadyAfterPolls = 2
	r := &Reconciler{Store: rec, Kube: fake, Transitions: rec, Timeout: time.Minute}
	ctx := context.Background()
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if rec.deployments["d1"].Status != domain.DeployProgressing {
		t.Fatalf("after pass 1: %s", rec.deployments["d1"].Status)
	}
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if rec.deployments["d1"].Status != domain.DeployReady {
		t.Fatalf("after pass 2: %s (calls %v)", rec.deployments["d1"].Status, rec.calls)
	}
	if spec, _, ok := fake.Applied("modeldock-staging", "iris"); !ok || spec.DeploymentID != "d1" {
		t.Fatal("workload not applied")
	}
	// Degradation of a live deployment is reported once, then recovery.
	_ = r.ReconcileOnce(ctx) // first observation: healthy, not reported
	fake.SetDegraded("modeldock-staging", "iris", true)
	_ = r.ReconcileOnce(ctx)
	_ = r.ReconcileOnce(ctx)
	fake.SetDegraded("modeldock-staging", "iris", false)
	_ = r.ReconcileOnce(ctx)
	if strings.Join(rec.health, ",") != "degraded,healthy" {
		t.Fatalf("health reports = %v", rec.health)
	}
}

func TestReconcilerTimeout(t *testing.T) {
	rec, fake := newRecorder(), NewFake()
	fake.OutcomeFor = func(WorkloadSpec) Outcome { return NeverReady }
	now := time.Now()
	r := &Reconciler{Store: rec, Kube: fake, Transitions: rec, Timeout: time.Minute, Now: func() time.Time { return now }}
	ctx := context.Background()
	_ = r.ReconcileOnce(ctx)
	if rec.deployments["d1"].Status != domain.DeployProgressing {
		t.Fatalf("status = %s", rec.deployments["d1"].Status)
	}
	now = now.Add(2 * time.Minute)
	_ = r.ReconcileOnce(ctx)
	if rec.deployments["d1"].Status != domain.DeployFailed {
		t.Fatalf("status = %s, want failed after timeout", rec.deployments["d1"].Status)
	}
	if last := rec.calls[len(rec.calls)-1]; !strings.Contains(last, "not ready within 1m0s") {
		t.Fatalf("reason = %s", last)
	}
}

func TestReconcilerFailFastAndApplyRetry(t *testing.T) {
	rec, fake := newRecorder(), NewFake()
	fake.ApplyErr = errors.New("connection refused")
	r := &Reconciler{Store: rec, Kube: fake, Transitions: rec, Timeout: time.Minute}
	ctx := context.Background()
	_ = r.ReconcileOnce(ctx)
	if rec.deployments["d1"].Status != domain.DeployPending {
		t.Fatal("apply error within timeout should be retried, not failed")
	}
	fake.ApplyErr = nil
	fake.OutcomeFor = func(WorkloadSpec) Outcome { return FailFast }
	_ = r.ReconcileOnce(ctx)
	if rec.deployments["d1"].Status != domain.DeployFailed {
		t.Fatalf("status = %s, want failed on ProgressDeadlineExceeded", rec.deployments["d1"].Status)
	}
}
