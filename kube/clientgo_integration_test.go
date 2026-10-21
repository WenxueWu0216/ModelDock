package kube

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/wenxuewu0216/modeldock/domain"
)

// TestClientGoAgainstRealCluster exercises ClientGo against a real API
// server. It is skipped unless MODELDOCK_TEST_KUBECONFIG is set.
//
// Set MODELDOCK_TEST_EXPECT_READY=1 when the cluster can actually run pods
// (kind, k3s, cloud) and MODELDOCK_TEST_IMAGE to a pullable model-server
// image; the test then waits for the rollout to complete. Without it the
// test only checks apply/watch/status plumbing, which works even against a
// control plane that cannot start containers.
func TestClientGoAgainstRealCluster(t *testing.T) {
	kc := os.Getenv("MODELDOCK_TEST_KUBECONFIG")
	if kc == "" {
		t.Skip("MODELDOCK_TEST_KUBECONFIG not set; skipping real-cluster test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cfg, err := RESTConfig(kc)
	if err != nil {
		t.Fatal(err)
	}
	ns := fmt.Sprintf("modeldock-it-%d", time.Now().Unix())
	c, err := NewClientGo(ctx, cfg, []string{ns})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cs := kubernetes.NewForConfigOrDie(cfg)
	t.Cleanup(func() {
		_ = cs.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
	})

	image := os.Getenv("MODELDOCK_TEST_IMAGE")
	if image == "" {
		image = "modeldock/iris-server:dev"
	}
	spec := WorkloadSpec{
		Namespace: ns, Name: "iris", Environment: domain.EnvStaging, Version: "1.0.0",
		Image: image, Port: 8080,
		Resources:    domain.Resources{CPURequest: "50m", MemoryRequest: "64Mi", MemoryLimit: "256Mi"},
		Scaling:      domain.Scaling{MinReplicas: 1, MaxReplicas: 2, TargetCPUPercentage: 70},
		DeploymentID: "it-dep-1", Revision: 1, ProgressDeadlineSeconds: 60,
	}
	if err := c.Apply(ctx, spec); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Idempotent re-apply (reconciler retries after a crash).
	if err := c.Apply(ctx, spec); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	for _, check := range []func() error{
		func() error { _, err := cs.CoreV1().Services(ns).Get(ctx, "iris", metav1.GetOptions{}); return err },
		func() error {
			_, err := cs.AutoscalingV2().HorizontalPodAutoscalers(ns).Get(ctx, "iris", metav1.GetOptions{})
			return err
		},
	} {
		if err := check(); err != nil {
			t.Fatalf("object missing after apply: %v", err)
		}
	}

	expectReady := os.Getenv("MODELDOCK_TEST_EXPECT_READY") == "1"
	deadline := time.Now().Add(90 * time.Second)
	var st WorkloadStatus
	for time.Now().Before(deadline) {
		st, err = c.Status(ctx, ns, "iris")
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if st.Exists && st.DeploymentID == "it-dep-1" && st.Observed >= st.Generation && !expectReady {
			break
		}
		if expectReady && st.RolloutComplete("it-dep-1") {
			break
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("observed status: %+v", st)
	if !st.Exists || st.DeploymentID != "it-dep-1" {
		t.Fatalf("status not observed via watch: %+v", st)
	}
	if expectReady && !st.RolloutComplete("it-dep-1") {
		t.Fatalf("rollout did not complete: %+v", st)
	}

	// A second revision bumps the generation and is tracked by its own ID.
	spec.DeploymentID, spec.Revision, spec.Version = "it-dep-2", 2, "1.0.1"
	if err := c.Apply(ctx, spec); err != nil {
		t.Fatalf("apply rev 2: %v", err)
	}
	for i := 0; i < 30; i++ {
		if st, _ = c.Status(ctx, ns, "iris"); st.DeploymentID == "it-dep-2" {
			break
		}
		time.Sleep(time.Second)
	}
	if st.DeploymentID != "it-dep-2" || st.RolloutComplete("it-dep-1") {
		t.Fatalf("revision 2 not observed: %+v", st)
	}
	if err := c.Delete(ctx, ns, "iris"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}
