package controller

import (
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	cachev1 "x1kun.com/simplecache-operator/api/v1"
)

func defaults(cr *cachev1.SimpleCache) {
	if cr.Spec.Size == 0 {
		cr.Spec.Size = 3
	}
	if cr.Spec.CacheBytes == 0 {
		cr.Spec.CacheBytes = 64 << 20
	}
	if cr.Spec.TTLSeconds == 0 {
		cr.Spec.TTLSeconds = 60
	}
}
func labels(cr *cachev1.SimpleCache) map[string]string { return map[string]string{"app": cr.Name} }
func metadata(cr *cachev1.SimpleCache, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: cr.Namespace}
}
func peerService(cr *cachev1.SimpleCache) *corev1.Service {
	return &corev1.Service{ObjectMeta: metadata(cr, cr.Name+"-svc"), Spec: corev1.ServiceSpec{
		Type: corev1.ServiceTypeClusterIP, ClusterIP: corev1.ClusterIPNone, Selector: labels(cr),
		Ports: []corev1.ServicePort{{Name: "peer", Port: 8001, TargetPort: intstr.FromInt32(8001), Protocol: corev1.ProtocolTCP}},
	}}
}
func apiService(cr *cachev1.SimpleCache) *corev1.Service {
	return &corev1.Service{ObjectMeta: metadata(cr, cr.Name+"-api"), Spec: corev1.ServiceSpec{
		Type: corev1.ServiceTypeClusterIP, Selector: labels(cr), Ports: []corev1.ServicePort{{Name: "api", Port: 9999, TargetPort: intstr.FromInt32(9999), Protocol: corev1.ProtocolTCP}},
	}}
}
func account(cr *cachev1.SimpleCache) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: metadata(cr, cr.Name+"-cache")}
}
func discoveryRole(cr *cachev1.SimpleCache) *rbacv1.Role {
	return &rbacv1.Role{ObjectMeta: metadata(cr, cr.Name+"-discovery"), Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: []string{"get", "list", "watch"}},
	}}
}
func discoveryBinding(cr *cachev1.SimpleCache) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{ObjectMeta: metadata(cr, cr.Name+"-discovery"), RoleRef: rbacv1.RoleRef{
		APIGroup: rbacv1.GroupName, Kind: "Role", Name: cr.Name + "-discovery",
	}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: cr.Name + "-cache", Namespace: cr.Namespace}}}
}
func statefulSet(cr *cachev1.SimpleCache) *appsv1.StatefulSet {
	resources := resourceDefaults(cr.Spec.Resources)
	grace := int64(10)
	env := []corev1.EnvVar{
		{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"}}},
		{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}},
		{Name: "DISCOVERY_MODE", Value: "kubernetes"},
		{Name: "PEER_SERVICE", Value: cr.Name + "-svc"},
		{Name: "CACHE_BYTES", Value: strconv.FormatInt(cr.Spec.CacheBytes, 10)},
		{Name: "TTL_SECONDS", Value: strconv.Itoa(int(cr.Spec.TTLSeconds))},
	}
	if cr.Spec.SourceSecret != "" {
		env = append(env, corev1.EnvVar{Name: "SOURCE_MODE", Value: "postgres"})
		for _, name := range []string{"DATABASE_URL", "CACHE_ADMIN_TOKEN"} {
			env = append(env, corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: cr.Spec.SourceSecret},
					Key:                  name,
				},
			}})
		}
	}
	probe := func(path string) *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt32(9999), Scheme: corev1.URISchemeHTTP},
		}, PeriodSeconds: 2, TimeoutSeconds: 1, FailureThreshold: 3, SuccessThreshold: 1}
	}
	startup := probe("/healthz")
	startup.FailureThreshold = 30
	return &appsv1.StatefulSet{ObjectMeta: metadata(cr, cr.Name), Spec: appsv1.StatefulSetSpec{
		Replicas: &cr.Spec.Size, ServiceName: cr.Name + "-svc", PodManagementPolicy: appsv1.ParallelPodManagement,
		Selector: &metav1.LabelSelector{MatchLabels: labels(cr)},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels(cr), Annotations: map[string]string{
			"prometheus.io/scrape": "true", "prometheus.io/port": "9999", "prometheus.io/path": "/metrics",
		}}, Spec: corev1.PodSpec{ServiceAccountName: cr.Name + "-cache", TerminationGracePeriodSeconds: &grace,
			Containers: []corev1.Container{{Name: "cache", Image: cr.Spec.Image, ImagePullPolicy: corev1.PullIfNotPresent,
				Command: []string{"./geecache-server", "-port=8001", "-api=true"},
				Ports:   []corev1.ContainerPort{{Name: "peer", ContainerPort: 8001, Protocol: corev1.ProtocolTCP}, {Name: "api", ContainerPort: 9999, Protocol: corev1.ProtocolTCP}},
				Env:     env, Resources: resources, StartupProbe: startup, LivenessProbe: probe("/healthz"), ReadinessProbe: probe("/readyz"),
			}},
		}},
	}}
}
