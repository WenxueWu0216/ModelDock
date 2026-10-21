package kube

import (
	"bytes"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"

	"github.com/wenxuewu0216/modeldock/domain"
)

// Objects is the rendered set of Kubernetes objects for one workload.
type Objects struct {
	Deployment *appsv1.Deployment
	Service    *corev1.Service
	HPA        *autoscalingv2.HorizontalPodAutoscaler
}

func buildResources(r domain.Resources) (corev1.ResourceRequirements, error) {
	return resourcesFor(WorkloadSpec{Resources: r})
}

func ptr[T any](v T) *T { return &v }

// Build renders the objects for spec. replicas is the Deployment replica
// count to apply (the HPA adjusts it afterwards); pass 0 to use
// spec.Scaling.MinReplicas.
func Build(spec WorkloadSpec, replicas int32) (Objects, error) {
	res, err := resourcesFor(spec)
	if err != nil {
		return Objects{}, err
	}
	if replicas < spec.Scaling.MinReplicas {
		replicas = spec.Scaling.MinReplicas
	}
	if replicas > spec.Scaling.MaxReplicas {
		replicas = spec.Scaling.MaxReplicas
	}
	selector := map[string]string{LabelName: spec.Name, LabelEnvironment: string(spec.Environment)}
	labels := map[string]string{
		LabelName:        spec.Name,
		LabelEnvironment: string(spec.Environment),
		LabelManagedBy:   ManagedByValue,
		LabelVersion:     spec.Version,
	}
	annotations := map[string]string{
		AnnotDeploymentID: spec.DeploymentID,
		AnnotRevision:     strconv.Itoa(spec.Revision),
	}
	podAnnotations := map[string]string{
		AnnotDeploymentID:      spec.DeploymentID,
		"prometheus.io/scrape": "true",
		"prometheus.io/port":   strconv.Itoa(int(spec.Port)),
		"prometheus.io/path":   "/metrics",
	}
	for k, v := range spec.Metadata {
		if isAnnotationKeySafe(k) {
			podAnnotations[AnnotMetadataPrefx+k] = v
		}
	}
	deadline := spec.ProgressDeadlineSeconds
	if deadline <= 0 {
		deadline = 300
	}
	port := corev1.ContainerPort{Name: "http", ContainerPort: spec.Port, Protocol: corev1.ProtocolTCP}
	probe := func(path string, initial int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: path, Port: intstr.FromString("http"),
			}},
			InitialDelaySeconds: initial, PeriodSeconds: 5, TimeoutSeconds: 2, FailureThreshold: 3,
		}
	}

	dep := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: spec.Namespace, Labels: labels, Annotations: annotations},
		Spec: appsv1.DeploymentSpec{
			Replicas:                &replicas,
			RevisionHistoryLimit:    ptr(int32(10)),
			ProgressDeadlineSeconds: &deadline,
			Selector:                &metav1.LabelSelector{MatchLabels: selector},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: ptr(intstr.FromInt32(0)),
					MaxSurge:       ptr(intstr.FromString("25%")),
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: podAnnotations},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr(true),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:           "model",
						Image:          spec.Image,
						Ports:          []corev1.ContainerPort{port},
						Env:            []corev1.EnvVar{{Name: "MODEL_VERSION", Value: spec.Version}, {Name: "PORT", Value: strconv.Itoa(int(spec.Port))}},
						Resources:      res,
						ReadinessProbe: probe("/health", 2),
						LivenessProbe:  probe("/health", 10),
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr(false),
							ReadOnlyRootFilesystem:   ptr(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}
	svc := &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: spec.Namespace, Labels: labels, Annotations: annotations},
		Spec: corev1.ServiceSpec{
			Selector: selector,
			Ports:    []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP}},
		},
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		TypeMeta:   metav1.TypeMeta{APIVersion: "autoscaling/v2", Kind: "HorizontalPodAutoscaler"},
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: spec.Namespace, Labels: labels, Annotations: annotations},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: spec.Name},
			MinReplicas:    ptr(spec.Scaling.MinReplicas),
			MaxReplicas:    spec.Scaling.MaxReplicas,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type: autoscalingv2.UtilizationMetricType, AverageUtilization: ptr(spec.Scaling.TargetCPUPercentage),
					},
				},
			}},
		},
	}
	return Objects{Deployment: dep, Service: svc, HPA: hpa}, nil
}

func resourcesFor(spec WorkloadSpec) (corev1.ResourceRequirements, error) {
	r := spec.Resources
	parse := func(field, v string) (resource.Quantity, bool, error) {
		if v == "" {
			return resource.Quantity{}, false, nil
		}
		q, err := resource.ParseQuantity(v)
		if err != nil {
			return q, false, fmt.Errorf("resources.%s %q: %w", field, v, err)
		}
		return q, true, nil
	}
	out := corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}}
	type item struct {
		field, val string
		list       corev1.ResourceList
		name       corev1.ResourceName
	}
	for _, it := range []item{
		{"cpuRequest", r.CPURequest, out.Requests, corev1.ResourceCPU},
		{"memoryRequest", r.MemoryRequest, out.Requests, corev1.ResourceMemory},
		{"cpuLimit", r.CPULimit, out.Limits, corev1.ResourceCPU},
		{"memoryLimit", r.MemoryLimit, out.Limits, corev1.ResourceMemory},
	} {
		q, ok, err := parse(it.field, it.val)
		if err != nil {
			return out, err
		}
		if ok {
			it.list[it.name] = q
		}
	}
	for name, lim := range out.Limits {
		if req, ok := out.Requests[name]; ok && lim.Cmp(req) < 0 {
			return out, fmt.Errorf("resources: %s limit %s is below request %s", name, lim.String(), req.String())
		}
	}
	if len(out.Limits) == 0 {
		out.Limits = nil
	}
	return out, nil
}

func isAnnotationKeySafe(k string) bool {
	if k == "" || len(k) > 40 {
		return false
	}
	for _, c := range k {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// RenderYAML renders spec as a multi-document YAML stream (Deployment,
// Service, HPA). It is used for `modeldock render` and kubeconform checks.
func RenderYAML(spec WorkloadSpec) ([]byte, error) {
	objs, err := Build(spec, 0)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	for i, o := range []any{objs.Deployment, objs.Service, objs.HPA} {
		b, err := yaml.Marshal(o)
		if err != nil {
			return nil, fmt.Errorf("kube: marshal object %d: %w", i, err)
		}
		if i > 0 {
			buf.WriteString("---\n")
		}
		buf.Write(b)
	}
	return buf.Bytes(), nil
}
