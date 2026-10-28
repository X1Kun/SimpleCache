package controller

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	cachev1 "x1kun.com/simplecache-operator/api/v1"
)

// Pinned to the APIs in Envoy Gateway v1.9.2 / Gateway API v1.6.1.
// Unstructured resources avoid pulling the entire gateway controller into this module.
var gatewayKinds = []schema.GroupVersionKind{
	{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway"},
	{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"},
	{Group: "gateway.envoyproxy.io", Version: "v1alpha1", Kind: "EnvoyProxy"},
	{Group: "gateway.envoyproxy.io", Version: "v1alpha1", Kind: "Backend"},
	{Group: "gateway.envoyproxy.io", Version: "v1alpha1", Kind: backendPolicyKind},
}

const backendPolicyKind = "BackendTrafficPolicy"

const gatewayController = "gateway.envoyproxy.io/gatewayclass-controller"

func gatewayObject(gvk schema.GroupVersionKind, namespace, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	return obj
}
func gatewayResource(cr *cachev1.SimpleCache, kind string, spec map[string]any) *unstructured.Unstructured {
	for _, gvk := range gatewayKinds {
		if gvk.Kind == kind {
			obj := gatewayObject(gvk, cr.Namespace, cr.Name)
			obj.Object["spec"] = spec
			return obj
		}
	}
	panic("unknown gateway kind")
}

func shardService(cr *cachev1.SimpleCache, ordinal int32) *corev1.Service {
	svc := apiService(cr)
	svc.Name = cr.Name + "-shard-" + strconv.Itoa(int(ordinal))
	svc.Spec.Selector["statefulset.kubernetes.io/pod-name"] = cr.Name + "-" + strconv.Itoa(int(ordinal))
	return svc
}

func gatewayResources(cr *cachev1.SimpleCache, ips []string) []*unstructured.Unstructured {
	endpoints := make([]any, 0, len(ips))
	for _, ip := range ips {
		endpoints = append(endpoints, map[string]any{"ip": map[string]any{"address": ip, "port": int64(9999)}})
	}
	return []*unstructured.Unstructured{
		gatewayResource(cr, "EnvoyProxy", map[string]any{"provider": map[string]any{
			"type": "Kubernetes", "kubernetes": map[string]any{
				"envoyDeployment": map[string]any{"replicas": int64(cr.Spec.Gateway.Replicas)},
				"envoyService":    map[string]any{"type": "ClusterIP"},
			},
		}}),
		gatewayResource(cr, "Backend", map[string]any{"endpoints": endpoints}),
		gatewayResource(cr, "Gateway", map[string]any{
			"gatewayClassName": cr.Spec.Gateway.ClassName,
			"infrastructure":   map[string]any{"parametersRef": map[string]any{"group": "gateway.envoyproxy.io", "kind": "EnvoyProxy", "name": cr.Name}},
			"listeners":        []any{map[string]any{"name": "http", "protocol": "HTTP", "port": int64(80), "allowedRoutes": map[string]any{"namespaces": map[string]any{"from": "Same"}}}},
		}),
		gatewayResource(cr, "HTTPRoute", map[string]any{
			"parentRefs": []any{map[string]any{"name": cr.Name, "sectionName": "http"}},
			"rules": []any{map[string]any{
				"matches":     []any{map[string]any{"path": map[string]any{"type": "Exact", "value": "/api"}}, map[string]any{"path": map[string]any{"type": "Exact", "value": "/invalidate"}}},
				"backendRefs": []any{map[string]any{"group": "gateway.envoyproxy.io", "kind": "Backend", "name": cr.Name}},
			}},
		}),
		gatewayResource(cr, backendPolicyKind, map[string]any{
			"targetRefs":   []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": cr.Name}},
			"loadBalancer": map[string]any{"type": "ConsistentHash", "consistentHash": map[string]any{"type": "QueryParams", "queryParams": []any{map[string]any{"name": "key"}}, "tableSize": int64(65537)}},
			"healthCheck":  map[string]any{"active": map[string]any{"type": "HTTP", "interval": "2s", "timeout": "1s", "healthyThreshold": int64(1), "unhealthyThreshold": int64(1), "http": map[string]any{"path": "/readyz", "method": "GET", "expectedStatuses": []any{int64(200)}}}},
		}),
	}
}

// desiredSubset preserves API defaulting, but replaces the spec on owned-field
// drift. Arrays are exact: obsolete backend addresses must never survive scaling.
func desiredSubset(actual, desired any) bool {
	switch d := desired.(type) {
	case map[string]any:
		a, ok := actual.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range d {
			if !desiredSubset(a[k], v) {
				return false
			}
		}
		return true
	case []any:
		a, ok := actual.([]any)
		if !ok || len(a) != len(d) {
			return false
		}
		for i := range d {
			if !desiredSubset(a[i], d[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(actual, desired)
	}
}

func (r *SimpleCacheReconciler) checkGatewayClass(ctx context.Context, cr *cachev1.SimpleCache) error {
	if !r.EnableGateway {
		return fmt.Errorf("enable --enable-gateway after installing Envoy Gateway v1.9.2 and its CRDs")
	}
	class := gatewayObject(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "GatewayClass"}, "", cr.Spec.Gateway.ClassName)
	if err := r.Get(ctx, client.ObjectKeyFromObject(class), class); err != nil {
		return fmt.Errorf("read GatewayClass: %w", err)
	}
	controller, _, _ := unstructured.NestedString(class.Object, "spec", "controllerName")
	if controller != gatewayController {
		return fmt.Errorf("GatewayClass must use %s", gatewayController)
	}
	if !conditionTrue(class, "Accepted", class.GetGeneration(), "status", "conditions") {
		return fmt.Errorf("GatewayClass is not accepted yet")
	}
	return nil
}

// reconcileGateway returns a readiness message, or empty once all owned resources
// have current status. Service ClusterIPs provide stable shard identities across
// Pod replacement; each Service selects exactly one StatefulSet ordinal.
func (r *SimpleCacheReconciler) reconcileGateway(ctx context.Context, cr *cachev1.SimpleCache) (string, error) {
	ips := make([]string, 0, cr.Spec.Size)
	for i := int32(0); i < cr.Spec.Size; i++ {
		svc := shardService(cr, i)
		if err := r.ensure(ctx, cr, svc); err != nil {
			return "", err
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(svc), svc); err != nil {
			return "", err
		}
		if svc.Spec.ClusterIP == "" {
			return "Waiting for shard Service addresses", nil
		}
		ips = append(ips, svc.Spec.ClusterIP)
	}
	objects := gatewayResources(cr, ips)
	for _, obj := range objects {
		if err := r.ensure(ctx, cr, obj); err != nil {
			return "", err
		}
	}
	if err := r.removeShardServices(ctx, cr, cr.Spec.Size); err != nil {
		return "", err
	}
	for _, obj := range objects {
		if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			return "", err
		}
		ready := true
		switch obj.GetKind() {
		case "Gateway":
			ready = conditionTrue(obj, "Accepted", obj.GetGeneration(), "status", "conditions") && conditionTrue(obj, "Programmed", obj.GetGeneration(), "status", "conditions")
		case "Backend":
			ready = conditionTrue(obj, "Accepted", obj.GetGeneration(), "status", "conditions")
		case "HTTPRoute":
			ready = parentConditionsTrue(obj, cr, "parents", true)
		case backendPolicyKind:
			ready = parentConditionsTrue(obj, cr, "ancestors", false)
		}
		if !ready {
			return "Waiting for " + obj.GetKind() + " acceptance/programming", nil
		}
	}
	return "", nil
}

func conditionTrue(obj *unstructured.Unstructured, kind string, generation int64, path ...string) bool {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, path...)
	for _, raw := range conditions {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if c["type"] == kind && c["status"] == "True" && c["observedGeneration"] == generation {
			return true
		}
	}
	return false
}
func parentConditionsTrue(obj *unstructured.Unstructured, cr *cachev1.SimpleCache, field string, resolved bool) bool {
	parents, _, _ := unstructured.NestedSlice(obj.Object, "status", field)
	for _, raw := range parents {
		p, ok := raw.(map[string]any)
		if !ok || p["controllerName"] != gatewayController {
			continue
		}
		refField := "parentRef"
		if field == "ancestors" {
			refField = "ancestorRef"
		}
		ref, ok := p[refField].(map[string]any)
		if !ok || ref["name"] != cr.Name {
			continue
		}
		if ns, ok := ref["namespace"]; ok && ns != cr.Namespace {
			continue
		}
		status := &unstructured.Unstructured{Object: p}
		if conditionTrue(status, "Accepted", obj.GetGeneration(), "conditions") && (!resolved || conditionTrue(status, "ResolvedRefs", obj.GetGeneration(), "conditions")) {
			return true
		}
	}
	return false
}

func (r *SimpleCacheReconciler) removeShardServices(ctx context.Context, cr *cachev1.SimpleCache, keep int32) error {
	var svcs corev1.ServiceList
	if err := r.List(ctx, &svcs, client.InNamespace(cr.Namespace)); err != nil {
		return err
	}
	for i := range svcs.Items {
		svc := &svcs.Items[i]
		if !metav1.IsControlledBy(svc, cr) || !strings.HasPrefix(svc.Name, cr.Name+"-shard-") {
			continue
		}
		suffix := strings.TrimPrefix(svc.Name, cr.Name+"-shard-")
		ordinal, err := strconv.Atoi(suffix)
		if err == nil && ordinal >= int(keep) {
			if err := r.Delete(ctx, svc); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	return nil
}
func (r *SimpleCacheReconciler) removeGateway(ctx context.Context, cr *cachev1.SimpleCache) error {
	if !r.EnableGateway {
		return nil
	}
	for _, gvk := range gatewayKinds {
		obj := gatewayObject(gvk, cr.Namespace, cr.Name)
		if err := r.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}, obj); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return err
			}
			continue
		}
		if metav1.IsControlledBy(obj, cr) {
			if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	return r.removeShardServices(ctx, cr, 0)
}
