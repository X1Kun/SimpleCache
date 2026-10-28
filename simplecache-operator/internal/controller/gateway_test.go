package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	cachev1 "x1kun.com/simplecache-operator/api/v1"
)

func TestDesiredSubsetDefaultsAndEndpointRemoval(t *testing.T) {
	if !desiredSubset(map[string]any{"owned": "yes", "default": "keep"}, map[string]any{"owned": "yes"}) {
		t.Fatal("API defaults caused drift")
	}
	if desiredSubset([]any{"a", "b"}, []any{"a"}) {
		t.Fatal("obsolete backend retained")
	}
	cr := &cachev1.SimpleCache{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"}, Spec: cachev1.SimpleCacheSpec{Image: "cache", Gateway: &cachev1.GatewaySpec{}}}
	defaults(cr)
	if err := validateSpec(cr); err != nil {
		t.Fatal(err)
	}
	cr.Spec.HotCacheBytes = 1024
	if err := validateSpec(cr); err == nil {
		t.Fatal("gateway accepted hot replicas")
	}
}

var _ = Describe("Gateway integration", func() {
	It("validates official CRDs, preserves allocated shard IPs, reconciles drift, status and scale down", func() {
		ctx := context.Background()
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "gateway-test-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		class := gatewayObject(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "GatewayClass"}, "", ns.Name)
		class.Object["spec"] = map[string]any{"controllerName": gatewayController}
		Expect(k8sClient.Create(ctx, class)).To(Succeed())
		defer func() {
			Expect(k8sClient.Delete(ctx, class)).To(Succeed())
			Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
		}()
		setConditions := func(obj *unstructured.Unstructured, kinds ...string) {
			conds := make([]any, 0, len(kinds))
			for _, kind := range kinds {
				conds = append(conds, map[string]any{"type": kind, "status": "True", "reason": "Accepted", "message": "Test status", "observedGeneration": obj.GetGeneration(), "lastTransitionTime": time.Now().UTC().Format(time.RFC3339)})
			}
			obj.Object["status"] = map[string]any{"conditions": conds}
			Expect(k8sClient.Status().Update(ctx, obj)).To(Succeed())
		}
		setConditions(class, "Accepted")
		cr := &cachev1.SimpleCache{ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: ns.Name}, Spec: cachev1.SimpleCacheSpec{Size: 3, Image: "cache:test", Gateway: &cachev1.GatewaySpec{ClassName: ns.Name, Replicas: 2}}}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		key := client.ObjectKeyFromObject(cr)
		r := &SimpleCacheReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		reconcile := func() {
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}
		reconcile()
		Expect(k8sClient.Get(ctx, key, cr)).To(Succeed())
		Expect(meta.FindStatusCondition(cr.Status.Conditions, "Ready").Reason).To(Equal("GatewayUnavailable"))
		r.EnableGateway = true
		reconcile()
		ips := make([]string, 0, 3)
		for i := range 3 {
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: fmt.Sprintf("cache-shard-%d", i), Namespace: ns.Name}, svc)).To(Succeed())
			ips = append(ips, svc.Spec.ClusterIP)
			Expect(svc.Spec.Selector["statefulset.kubernetes.io/pod-name"]).To(Equal(fmt.Sprintf("cache-%d", i)))
		}
		// API defaults must not cause endless resource updates.
		objects := gatewayResources(cr, ips)
		versions := map[string]string{}
		for _, obj := range objects {
			Expect(k8sClient.Get(ctx, key, obj)).To(Succeed())
			versions[obj.GetKind()] = obj.GetResourceVersion()
		}
		reconcile()
		for _, obj := range objects {
			Expect(k8sClient.Get(ctx, key, obj)).To(Succeed())
			Expect(obj.GetResourceVersion()).To(Equal(versions[obj.GetKind()]))
		}
		// A policy being unaccepted must keep the overall cache unready.
		Expect(k8sClient.Get(ctx, key, cr)).To(Succeed())
		Expect(meta.FindStatusCondition(cr.Status.Conditions, "Ready").Status).To(Equal(metav1.ConditionFalse))
		for _, obj := range objects {
			switch obj.GetKind() {
			case "Gateway":
				setConditions(obj, "Accepted", "Programmed")
			case "Backend":
				setConditions(obj, "Accepted")
			case "HTTPRoute", "BackendTrafficPolicy":
				conds := []any{map[string]any{"type": "Accepted", "status": "True", "reason": "Accepted", "message": "Test", "observedGeneration": obj.GetGeneration(), "lastTransitionTime": time.Now().UTC().Format(time.RFC3339)}}
				field, refField := "parents", "parentRef"
				if obj.GetKind() == "BackendTrafficPolicy" {
					field, refField = "ancestors", "ancestorRef"
				} else {
					conds = append(conds, map[string]any{"type": "ResolvedRefs", "status": "True", "reason": "ResolvedRefs", "message": "Test", "observedGeneration": obj.GetGeneration(), "lastTransitionTime": time.Now().UTC().Format(time.RFC3339)})
				}
				obj.Object["status"] = map[string]any{field: []any{map[string]any{refField: map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": cr.Name, "namespace": ns.Name}, "controllerName": gatewayController, "conditions": conds}}}
				Expect(k8sClient.Status().Update(ctx, obj)).To(Succeed())
			}
		}
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, sts)).To(Succeed())
		sts.Status.Replicas = 3
		sts.Status.ObservedGeneration = sts.Generation
		sts.Status.ReadyReplicas = 3
		sts.Status.UpdatedReplicas = 3
		Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
		reconcile()
		Expect(k8sClient.Get(ctx, key, cr)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(cr.Status.Conditions, "Ready")).To(BeTrue())
		cr.Spec.Size = 2
		Expect(k8sClient.Update(ctx, cr)).To(Succeed())
		reconcile()
		svc := shardService(cr, 0)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(svc), svc)).To(Succeed())
		Expect(svc.Spec.ClusterIP).To(Equal(ips[0]))
		extra := shardService(cr, 2)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(extra), extra)).NotTo(Succeed())
		backend := gatewayResource(cr, "Backend", nil)
		Expect(k8sClient.Get(ctx, key, backend)).To(Succeed())
		endpoints, _, _ := unstructured.NestedSlice(backend.Object, "spec", "endpoints")
		Expect(endpoints).To(HaveLen(2))
		// Child watch restores a drifted policy without relying on periodic polling.
		skipNameValidation := true
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: k8sClient.Scheme(), Controller: controllerconfig.Controller{SkipNameValidation: &skipNameValidation}, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
		Expect(err).NotTo(HaveOccurred())
		watched := &SimpleCacheReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), EnableGateway: true}
		Expect(watched.SetupWithManager(mgr)).To(Succeed())
		managerCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- mgr.Start(managerCtx) }()
		managerStopped := false
		defer func() {
			if !managerStopped {
				stop()
				Eventually(done, 10*time.Second).Should(Receive(Succeed()))
			}
		}()
		Expect(mgr.GetCache().WaitForCacheSync(managerCtx)).To(BeTrue())
		policy := gatewayResource(cr, "BackendTrafficPolicy", nil)
		Expect(k8sClient.Get(ctx, key, policy)).To(Succeed())
		Expect(unstructured.SetNestedField(policy.Object, "RoundRobin", "spec", "loadBalancer", "type")).To(Succeed())
		unstructured.RemoveNestedField(policy.Object, "spec", "loadBalancer", "consistentHash")
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())
		Eventually(func() string {
			if err := k8sClient.Get(ctx, key, policy); err != nil {
				return ""
			}
			v, _, _ := unstructured.NestedString(policy.Object, "spec", "loadBalancer", "type")
			return v
		}, 10*time.Second, 100*time.Millisecond).Should(Equal("ConsistentHash"))
		stop()
		Expect(<-done).To(Succeed())
		managerStopped = true
		// Disabling Gateway removes only owned gateway resources and keeps legacy mode.
		Expect(k8sClient.Get(ctx, key, cr)).To(Succeed())
		cr.Spec.Gateway = nil
		Expect(k8sClient.Update(ctx, cr)).To(Succeed())
		reconcile()
		Expect(k8sClient.Get(ctx, key, backend)).NotTo(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(class), class)).To(Succeed())
	})
})
