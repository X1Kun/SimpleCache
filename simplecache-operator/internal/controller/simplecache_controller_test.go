/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"reflect"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	cachev1 "x1kun.com/simplecache-operator/api/v1"
)

const testNamespace = "default"

var _ = Describe("SimpleCache controller contract", func() {
	const resourceName = "test-resource"
	key := types.NamespacedName{Name: resourceName, Namespace: testNamespace}
	ctx := context.Background()
	var cr *cachev1.SimpleCache
	var r *SimpleCacheReconciler
	var stopManager context.CancelFunc
	var managerDone chan error
	request := reconcile.Request{NamespacedName: key}
	getCR := func() { Expect(k8sClient.Get(ctx, key, cr)).To(Succeed()) }
	reconcileNow := func() { _, err := r.Reconcile(ctx, request); Expect(err).NotTo(HaveOccurred()) }

	BeforeEach(func() {
		cr = &cachev1.SimpleCache{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: testNamespace}, Spec: cachev1.SimpleCacheSpec{Size: 3, Image: "simplecache:envtest"}}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		getCR()
		r = &SimpleCacheReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	})
	AfterEach(func() {
		if stopManager != nil {
			stopManager()
			Eventually(managerDone, 5*time.Second).Should(Receive(Succeed()))
			stopManager = nil
		}
		// Envtest has no garbage collector, so remove every child explicitly.
		children := []client.Object{
			&appsv1.StatefulSet{ObjectMeta: metadata(cr, resourceName)},
			&corev1.Service{ObjectMeta: metadata(cr, resourceName+"-svc")},
			&corev1.Service{ObjectMeta: metadata(cr, resourceName+"-api")},
			&corev1.ServiceAccount{ObjectMeta: metadata(cr, resourceName+"-cache")},
			&rbacv1.RoleBinding{ObjectMeta: metadata(cr, resourceName+"-discovery")},
			&rbacv1.Role{ObjectMeta: metadata(cr, resourceName+"-discovery")},
		}
		for _, obj := range children {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
		getCR()
		Expect(k8sClient.Delete(ctx, cr)).To(Succeed())
	})

	It("reserves hot cache within the total budget and rejects an oversized reservation", func() {
		cr.Spec.HotCacheBytes = 1 << 20
		Expect(k8sClient.Update(ctx, cr)).To(Succeed())
		reconcileNow()
		var sts appsv1.StatefulSet
		Expect(k8sClient.Get(ctx, key, &sts)).To(Succeed())
		env := map[string]string{}
		for _, value := range sts.Spec.Template.Spec.Containers[0].Env {
			env[value.Name] = value.Value
		}
		Expect(env["CACHE_BYTES"]).To(Equal("67108864"))
		Expect(env["HOT_CACHE_BYTES"]).To(Equal("1048576"))
		getCR()
		cr.Spec.HotCacheBytes = cr.Spec.CacheBytes
		Expect(k8sClient.Update(ctx, cr)).To(Succeed())
		reconcileNow()
		getCR()
		Expect(meta.FindStatusCondition(cr.Status.Conditions, "Ready").Reason).To(Equal("InvalidSpec"))
	})

	It("wires PostgreSQL Secret references and removes them when returning to demo mode", func() {
		cr.Spec.SourceSecret = "product-source"
		Expect(k8sClient.Update(ctx, cr)).To(Succeed())
		reconcileNow()
		var sts appsv1.StatefulSet
		Expect(k8sClient.Get(ctx, key, &sts)).To(Succeed())
		env := map[string]corev1.EnvVar{}
		for _, value := range sts.Spec.Template.Spec.Containers[0].Env {
			env[value.Name] = value
		}
		Expect(env["SOURCE_MODE"].Value).To(Equal("postgres"))
		for _, name := range []string{"DATABASE_URL", "CACHE_ADMIN_TOKEN"} {
			Expect(env[name].Value).To(BeEmpty())
			Expect(env[name].ValueFrom).NotTo(BeNil())
			Expect(env[name].ValueFrom.SecretKeyRef.Name).To(Equal("product-source"))
			Expect(env[name].ValueFrom.SecretKeyRef.Key).To(Equal(name))
		}
		getCR()
		cr.Spec.SourceSecret = ""
		Expect(k8sClient.Update(ctx, cr)).To(Succeed())
		reconcileNow()
		Expect(k8sClient.Get(ctx, key, &sts)).To(Succeed())
		for _, value := range sts.Spec.Template.Spec.Containers[0].Env {
			Expect(value.Name).NotTo(BeElementOf("SOURCE_MODE", "DATABASE_URL", "CACHE_ADMIN_TOKEN"))
		}
	})

	It("reconciles discovery resources, status and replica-only scaling", func() {
		reconcileNow()
		var headless, api corev1.Service
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-svc", Namespace: testNamespace}, &headless)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-api", Namespace: testNamespace}, &api)).To(Succeed())
		Expect(headless.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
		Expect(headless.Spec.PublishNotReadyAddresses).To(BeFalse())
		Expect(headless.Spec.Ports).To(HaveLen(1))
		Expect(headless.Spec.Ports[0].Name).To(Equal("peer"))
		Expect(api.Spec.ClusterIP).NotTo(BeEmpty())
		Expect(metav1.IsControlledBy(&api, cr)).To(BeTrue())
		allocatedIP := api.Spec.ClusterIP

		var account corev1.ServiceAccount
		var role rbacv1.Role
		var binding rbacv1.RoleBinding
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-cache", Namespace: testNamespace}, &account)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-discovery", Namespace: testNamespace}, &role)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-discovery", Namespace: testNamespace}, &binding)).To(Succeed())
		Expect(role.Rules).To(Equal(discoveryRole(cr).Rules))
		Expect(binding.Subjects[0].Name).To(Equal(account.Name))
		Expect(metav1.IsControlledBy(&role, cr)).To(BeTrue())

		var sts appsv1.StatefulSet
		Expect(k8sClient.Get(ctx, key, &sts)).To(Succeed())
		Expect(sts.Spec.PodManagementPolicy).To(Equal(appsv1.ParallelPodManagement))
		container := sts.Spec.Template.Spec.Containers[0]
		Expect(sts.Spec.Template.Spec.ServiceAccountName).To(Equal(account.Name))
		Expect(container.ReadinessProbe.HTTPGet.Path).To(Equal("/readyz"))
		Expect(container.StartupProbe.HTTPGet.Path).To(Equal("/healthz"))
		environment := map[string]string{}
		for _, env := range container.Env {
			environment[env.Name] = env.Value
		}
		Expect(environment).NotTo(HaveKey("PEERS"))
		Expect(environment).NotTo(HaveKey("SELF_ADDR"))
		Expect(environment["DISCOVERY_MODE"]).To(Equal("kubernetes"))
		Expect(environment["PEER_SERVICE"]).To(Equal(headless.Name))

		By("reporting a progressing rollout without changing resources on repetition")
		getCR()
		Expect(meta.FindStatusCondition(cr.Status.Conditions, "Ready").Status).To(Equal(metav1.ConditionFalse))
		version, statusVersion := sts.ResourceVersion, cr.ResourceVersion
		reconcileNow()
		Expect(k8sClient.Get(ctx, key, &sts)).To(Succeed())
		getCR()
		Expect(sts.ResourceVersion).To(Equal(version))
		Expect(cr.ResourceVersion).To(Equal(statusVersion))

		By("reflecting an observed StatefulSet rollout")
		sts.Status.ObservedGeneration = sts.Generation
		sts.Status.Replicas = 3
		sts.Status.CurrentReplicas = 3
		sts.Status.ReadyReplicas = 3
		sts.Status.UpdatedReplicas = 3
		sts.Status.CurrentRevision = "revision-1"
		sts.Status.UpdateRevision = "revision-1"
		Expect(k8sClient.Status().Update(ctx, &sts)).To(Succeed())
		reconcileNow()
		getCR()
		Expect(meta.IsStatusConditionTrue(cr.Status.Conditions, "Ready")).To(BeTrue())
		Expect(cr.Status.ObservedGeneration).To(Equal(cr.Generation))

		original := sts.Spec.Template.DeepCopy()
		for _, size := range []int32{5, 2} {
			cr.Spec.Size = size
			Expect(k8sClient.Update(ctx, cr)).To(Succeed())
			reconcileNow()
			Expect(k8sClient.Get(ctx, key, &sts)).To(Succeed())
			Expect(*sts.Spec.Replicas).To(Equal(size))
			Expect(reflect.DeepEqual(original, &sts.Spec.Template)).To(BeTrue())
			getCR()
			Expect(meta.IsStatusConditionTrue(cr.Status.Conditions, "Ready")).To(BeFalse())
		}

		By("repairing Service drift while preserving its allocated address")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: api.Name, Namespace: testNamespace}, &api)).To(Succeed())
		api.Spec.Selector = map[string]string{"wrong": "selector"}
		Expect(k8sClient.Update(ctx, &api)).To(Succeed())
		reconcileNow()
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: api.Name, Namespace: testNamespace}, &api)).To(Succeed())
		Expect(api.Spec.ClusterIP).To(Equal(allocatedIP))
		Expect(api.Spec.Selector).To(Equal(labels(cr)))
		Expect(k8sClient.Delete(ctx, &api)).To(Succeed())
		reconcileNow()
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: api.Name, Namespace: testNamespace}, &api)).To(Succeed())

		By("rolling the image when desired configuration changes")
		getCR()
		cr.Spec.Image = "simplecache:envtest-v2"
		Expect(k8sClient.Update(ctx, cr)).To(Succeed())
		reconcileNow()
		Expect(k8sClient.Get(ctx, key, &sts)).To(Succeed())
		Expect(sts.Spec.Template.Spec.Containers[0].Image).To(Equal(cr.Spec.Image))
	})

	It("rejects unrelated same-name resources and reports the conflict", func() {
		foreign := &corev1.Service{ObjectMeta: metadata(cr, resourceName+"-svc"), Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8001}}}}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		original := foreign.DeepCopy()
		_, err := r.Reconcile(ctx, request)
		Expect(err).To(HaveOccurred())
		getCR()
		Expect(meta.FindStatusCondition(cr.Status.Conditions, "Ready").Reason).To(Equal("ResourceConflict"))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: foreign.Name, Namespace: testNamespace}, foreign)).To(Succeed())
		Expect(foreign.ResourceVersion).To(Equal(original.ResourceVersion))
		Expect(foreign.OwnerReferences).To(BeEmpty())
	})

	It("uses compatible defaults below an explicitly smaller memory limit", func() {
		cr.Spec.Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")}}
		Expect(k8sClient.Update(ctx, cr)).To(Succeed())
		reconcileNow()
		var sts appsv1.StatefulSet
		Expect(k8sClient.Get(ctx, key, &sts)).To(Succeed())
		q := sts.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]
		Expect(q.Cmp(resource.MustParse("64Mi"))).To(Equal(0))
	})

	It("ignores a missing primary resource", func() {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "missing-cache", Namespace: testNamespace}})
		Expect(err).NotTo(HaveOccurred())
		var sts appsv1.StatefulSet
		Expect(errors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: "missing-cache", Namespace: testNamespace}, &sts))).To(BeTrue())
	})

	It("reports invalid resource budgets without creating a workload", func() {
		cr.Spec.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
		}
		Expect(k8sClient.Update(ctx, cr)).To(Succeed())
		reconcileNow()
		getCR()
		Expect(meta.FindStatusCondition(cr.Status.Conditions, "Ready").Reason).To(Equal("InvalidSpec"))
		var sts appsv1.StatefulSet
		Expect(errors.IsNotFound(k8sClient.Get(ctx, key, &sts))).To(BeTrue())
	})

	It("repairs deleted children through the registered watch", func() {
		manager, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: k8sClient.Scheme(), Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
		Expect(err).NotTo(HaveOccurred())
		reconciler := &SimpleCacheReconciler{Client: manager.GetClient(), Scheme: manager.GetScheme()}
		Expect(reconciler.SetupWithManager(manager)).To(Succeed())
		managerCtx, cancelManager := context.WithCancel(context.Background())
		stopManager = cancelManager
		managerDone = make(chan error, 1)
		go func() { managerDone <- manager.Start(managerCtx) }()
		serviceKey := types.NamespacedName{Name: resourceName + "-api", Namespace: testNamespace}
		var svc corev1.Service
		Eventually(func() error { return k8sClient.Get(ctx, serviceKey, &svc) }, 10*time.Second, 50*time.Millisecond).Should(Succeed())
		previousUID := svc.UID
		Expect(k8sClient.Delete(ctx, &svc)).To(Succeed())
		Eventually(func() bool {
			if err := k8sClient.Get(ctx, serviceKey, &svc); err != nil {
				return false
			}
			return svc.UID != previousUID && metav1.IsControlledBy(&svc, cr)
		}, 10*time.Second, 50*time.Millisecond).Should(BeTrue())
	})
})
