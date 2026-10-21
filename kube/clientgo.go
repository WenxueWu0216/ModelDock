package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

// ClientGo is the real Client backed by client-go. Writes use server-side
// apply with a dedicated field manager. Reads come from a shared informer
// cache (a watch on ModelDock-managed Deployments), so the reconciler's
// polling never hits the API server directly.
type ClientGo struct {
	cs      kubernetes.Interface
	listers map[string]appslisters.DeploymentLister
	stop    chan struct{}
}

// RESTConfig loads in-cluster config, falling back to kubeconfig (path, or
// $KUBECONFIG / ~/.kube/config when empty).
func RESTConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kube: load kubeconfig: %w", err)
	}
	return cfg, nil
}

// NewClientGo builds a client watching Deployments in the given namespaces
// and waits for the informer caches to sync.
func NewClientGo(ctx context.Context, cfg *rest.Config, namespaces []string) (*ClientGo, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: build clientset: %w", err)
	}
	return newClientGo(ctx, cs, namespaces)
}

func newClientGo(ctx context.Context, cs kubernetes.Interface, namespaces []string) (*ClientGo, error) {
	c := &ClientGo{cs: cs, listers: map[string]appslisters.DeploymentLister{}, stop: make(chan struct{})}
	var synced []cache.InformerSynced
	for _, ns := range namespaces {
		f := informers.NewSharedInformerFactoryWithOptions(cs, 10*time.Minute,
			informers.WithNamespace(ns),
			informers.WithTweakListOptions(func(o *metav1.ListOptions) {
				o.LabelSelector = LabelManagedBy + "=" + ManagedByValue
			}))
		inf := f.Apps().V1().Deployments()
		c.listers[ns] = inf.Lister()
		synced = append(synced, inf.Informer().HasSynced)
		f.Start(c.stop)
	}
	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), synced...) {
		close(c.stop)
		return nil, errors.New("kube: informer caches did not sync within 30s (RBAC or connectivity?)")
	}
	return c, nil
}

// Close stops the informers.
func (c *ClientGo) Close() { close(c.stop) }

func (c *ClientGo) apply(ctx context.Context, ns, resource string, obj any,
	patch func(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, sub ...string) error,
	name string) error {
	data, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("kube: marshal %s %s/%s: %w", resource, ns, name, err)
	}
	force := true
	if err := patch(ctx, name, types.ApplyPatchType, data, metav1.PatchOptions{FieldManager: FieldManager, Force: &force}); err != nil {
		return fmt.Errorf("kube: apply %s %s/%s: %w", resource, ns, name, err)
	}
	return nil
}

// Apply implements Client via server-side apply of Service, HPA and
// Deployment. The replica count is carried over from the live object so a
// new revision does not undo HPA scaling.
func (c *ClientGo) Apply(ctx context.Context, spec WorkloadSpec) error {
	var replicas int32
	if live, err := c.cs.AppsV1().Deployments(spec.Namespace).Get(ctx, spec.Name, metav1.GetOptions{}); err == nil {
		if live.Spec.Replicas != nil {
			replicas = *live.Spec.Replicas
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: get deployment %s/%s: %w", spec.Namespace, spec.Name, err)
	}
	objs, err := Build(spec, replicas)
	if err != nil {
		return fmt.Errorf("kube: build manifests: %w", err)
	}
	if err := c.ensureNamespace(ctx, spec.Namespace); err != nil {
		return err
	}
	svcs := c.cs.CoreV1().Services(spec.Namespace)
	if err := c.apply(ctx, spec.Namespace, "service", objs.Service, func(ctx context.Context, n string, pt types.PatchType, d []byte, o metav1.PatchOptions, s ...string) error {
		_, err := svcs.Patch(ctx, n, pt, d, o, s...)
		return err
	}, spec.Name); err != nil {
		return err
	}
	hpas := c.cs.AutoscalingV2().HorizontalPodAutoscalers(spec.Namespace)
	if err := c.apply(ctx, spec.Namespace, "hpa", objs.HPA, func(ctx context.Context, n string, pt types.PatchType, d []byte, o metav1.PatchOptions, s ...string) error {
		_, err := hpas.Patch(ctx, n, pt, d, o, s...)
		return err
	}, spec.Name); err != nil {
		return err
	}
	deps := c.cs.AppsV1().Deployments(spec.Namespace)
	return c.apply(ctx, spec.Namespace, "deployment", objs.Deployment, func(ctx context.Context, n string, pt types.PatchType, d []byte, o metav1.PatchOptions, s ...string) error {
		_, err := deps.Patch(ctx, n, pt, d, o, s...)
		return err
	}, spec.Name)
}

func (c *ClientGo) ensureNamespace(ctx context.Context, ns string) error {
	_, err := c.cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: get namespace %s: %w", ns, err)
	}
	_, err = c.cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: ns, Labels: map[string]string{LabelManagedBy: ManagedByValue},
	}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("kube: create namespace %s: %w", ns, err)
	}
	return nil
}

// Status implements Client from the informer cache, falling back to a live
// GET when the object is not cached yet (just created) or the namespace is
// not watched.
func (c *ClientGo) Status(ctx context.Context, ns, name string) (WorkloadStatus, error) {
	var d *appsv1.Deployment
	if l, ok := c.listers[ns]; ok {
		if cached, err := l.Deployments(ns).Get(name); err == nil {
			d = cached
		}
	}
	if d == nil {
		live, err := c.cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return WorkloadStatus{}, nil
		}
		if err != nil {
			return WorkloadStatus{}, fmt.Errorf("kube: get deployment %s/%s: %w", ns, name, err)
		}
		d = live
	}
	return StatusFromDeployment(d), nil
}

// StatusFromDeployment converts a Deployment into a WorkloadStatus.
func StatusFromDeployment(d *appsv1.Deployment) WorkloadStatus {
	st := WorkloadStatus{
		Exists:       true,
		DeploymentID: d.Annotations[AnnotDeploymentID],
		Generation:   d.Generation,
		Observed:     d.Status.ObservedGeneration,
		Updated:      d.Status.UpdatedReplicas,
		Ready:        d.Status.ReadyReplicas,
		Available:    d.Status.AvailableReplicas,
		Total:        d.Status.Replicas,
		Desired:      1,
	}
	if d.Spec.Replicas != nil {
		st.Desired = *d.Spec.Replicas
	}
	// Conditions describe the generation the controller last observed. Right
	// after a new template is applied they can still carry the previous
	// rollout's ProgressDeadlineExceeded, so ignore them until observed.
	if st.Observed < st.Generation {
		return st
	}
	for _, cond := range d.Status.Conditions {
		switch {
		case cond.Type == appsv1.DeploymentProgressing && cond.Reason == "ProgressDeadlineExceeded":
			st.Failed, st.Message = true, "ProgressDeadlineExceeded: "+cond.Message
		case cond.Type == appsv1.DeploymentReplicaFailure && cond.Status == corev1.ConditionTrue:
			st.Failed, st.Message = true, "ReplicaFailure: "+cond.Message
		}
	}
	return st
}

// Delete implements Client.
func (c *ClientGo) Delete(ctx context.Context, ns, name string) error {
	var errs []error
	if err := c.cs.AppsV1().Deployments(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete deployment: %w", err))
	}
	if err := c.cs.CoreV1().Services(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete service: %w", err))
	}
	if err := c.cs.AutoscalingV2().HorizontalPodAutoscalers(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete hpa: %w", err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("kube: delete %s/%s: %w", ns, name, errors.Join(errs...))
	}
	return nil
}
