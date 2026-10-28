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
	"errors"
	"fmt"
	"reflect"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	cachev1 "x1kun.com/simplecache-operator/api/v1"
)

type SimpleCacheReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	EnableGateway bool
}

// +kubebuilder:rbac:groups=cache.x1kun.com,resources=simplecaches,verbs=get;list;watch
// +kubebuilder:rbac:groups=cache.x1kun.com,resources=simplecaches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cache.x1kun.com,resources=simplecaches/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways;httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.envoyproxy.io,resources=envoyproxies;backends;backendtrafficpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch

func (r *SimpleCacheReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cr cachev1.SimpleCache
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !cr.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	desired := cr.DeepCopy()
	defaults(desired)
	if err := validateSpec(desired); err != nil {
		if statusErr := r.setStatus(ctx, &cr, 0, metav1.ConditionFalse, "InvalidSpec", err.Error()); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, nil
	}
	if desired.Spec.Gateway != nil {
		if err := r.checkGatewayClass(ctx, desired); err != nil {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, r.setStatus(ctx, &cr, 0, metav1.ConditionFalse, "GatewayUnavailable", err.Error())
		}
	}
	objects := []client.Object{account(desired), discoveryRole(desired), discoveryBinding(desired), peerService(desired), apiService(desired), statefulSet(desired)}
	for _, wanted := range objects {
		if err := r.ensure(ctx, &cr, wanted); err != nil {
			reason := "ReconcileFailed"
			var conflict *ownershipConflict
			if errors.As(err, &conflict) {
				reason = "ResourceConflict"
			}
			if statusErr := r.setStatus(ctx, &cr, 0, metav1.ConditionFalse, reason, err.Error()); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			log.FromContext(ctx).Error(err, "Could not reconcile cache resource", "name", wanted.GetName())
			return ctrl.Result{}, err
		}
	}
	gatewayMessage := ""
	if desired.Spec.Gateway != nil {
		var err error
		gatewayMessage, err = r.reconcileGateway(ctx, desired)
		if err != nil {
			if statusErr := r.setStatus(ctx, &cr, 0, metav1.ConditionFalse, "GatewayReconcileFailed", err.Error()); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, err
		}
	} else if err := r.removeGateway(ctx, desired); err != nil {
		return ctrl.Result{}, err
	}
	var sts appsv1.StatefulSet
	if err := r.Get(ctx, req.NamespacedName, &sts); err != nil {
		return ctrl.Result{}, err
	}
	condition := metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Reconciling",
		Message: "Waiting for StatefulSet rollout", ObservedGeneration: cr.Generation,
	}
	if sts.Status.ObservedGeneration >= sts.Generation && sts.Status.ReadyReplicas == desired.Spec.Size && sts.Status.UpdatedReplicas == desired.Spec.Size && sts.Status.CurrentRevision == sts.Status.UpdateRevision {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "Ready"
		condition.Message = "All desired cache replicas are ready"
	}
	if gatewayMessage != "" {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "GatewayPending"
		condition.Message = gatewayMessage
	}
	result := ctrl.Result{}
	if desired.Spec.Gateway != nil {
		result.RequeueAfter = 5 * time.Second
	}
	return result, r.setStatus(ctx, &cr, sts.Status.ReadyReplicas, condition.Status, condition.Reason, condition.Message)
}

func (r *SimpleCacheReconciler) setStatus(ctx context.Context, cr *cachev1.SimpleCache, ready int32, status metav1.ConditionStatus, reason, message string) error {
	before := cr.DeepCopy()
	cr.Status.ObservedGeneration = cr.Generation
	cr.Status.ReadyReplicas = ready
	meta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{Type: "Ready", Status: status, Reason: reason, Message: message, ObservedGeneration: cr.Generation})
	if !reflect.DeepEqual(before.Status, cr.Status) {
		return r.Status().Patch(ctx, cr, client.MergeFrom(before))
	}
	return nil
}

type ownershipConflict struct{ message string }

func (e *ownershipConflict) Error() string { return e.message }

// ensure preserves allocated Service fields and rejects unrelated name collisions.
func (r *SimpleCacheReconciler) ensure(ctx context.Context, owner *cachev1.SimpleCache, wanted client.Object) error {
	actual := wanted.DeepCopyObject().(client.Object)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, actual, func() error {
		if actual.GetResourceVersion() != "" && !metav1.IsControlledBy(actual, owner) {
			return &ownershipConflict{message: fmt.Sprintf("%T %s is not owned by this SimpleCache", actual, actual.GetName())}
		}
		if err := ctrl.SetControllerReference(owner, actual, r.Scheme); err != nil {
			return err
		}
		switch current := actual.(type) {
		case *unstructured.Unstructured:
			spec := wanted.(*unstructured.Unstructured).Object["spec"]
			if !desiredSubset(current.Object["spec"], spec) {
				current.Object["spec"] = runtime.DeepCopyJSONValue(spec)
			}
		case *corev1.Service:
			spec := wanted.(*corev1.Service).Spec
			if spec.ClusterIP == corev1.ClusterIPNone && current.Spec.ClusterIP != "" && current.Spec.ClusterIP != corev1.ClusterIPNone {
				return fmt.Errorf("existing Service is not headless")
			}
			if spec.ClusterIP == corev1.ClusterIPNone {
				current.Spec.ClusterIP = corev1.ClusterIPNone
			}
			current.Spec.Selector = spec.Selector
			current.Spec.Ports = spec.Ports
			current.Spec.Type = spec.Type
			current.Spec.PublishNotReadyAddresses = false
		case *corev1.ServiceAccount:
		case *rbacv1.Role:
			current.Rules = wanted.(*rbacv1.Role).Rules
		case *rbacv1.RoleBinding:
			binding := wanted.(*rbacv1.RoleBinding)
			if current.GetResourceVersion() != "" && current.RoleRef != binding.RoleRef {
				return fmt.Errorf("RoleBinding roleRef is immutable")
			}
			current.RoleRef = binding.RoleRef
			current.Subjects = binding.Subjects
		case *appsv1.StatefulSet:
			spec := wanted.(*appsv1.StatefulSet).Spec
			if current.GetResourceVersion() == "" {
				current.Spec = spec
			} else {
				// Immutable serviceName/selector/management policy are preserved on existing clusters.
				if current.Spec.ServiceName != spec.ServiceName || !reflect.DeepEqual(current.Spec.Selector, spec.Selector) {
					return fmt.Errorf("StatefulSet immutable identity does not match")
				}
				current.Spec.Replicas = spec.Replicas
				mergeTemplate(&current.Spec.Template, &spec.Template)
			}
		}
		return nil
	})
	return err
}
func (r *SimpleCacheReconciler) SetupWithManager(manager ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(manager).For(&cachev1.SimpleCache{}).
		Owns(&appsv1.StatefulSet{}).Owns(&corev1.Service{}).Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.Role{}).Owns(&rbacv1.RoleBinding{})
	if r.EnableGateway {
		for _, gvk := range gatewayKinds {
			if _, err := manager.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
				return fmt.Errorf("install gateway CRDs before --enable-gateway: %w", err)
			}
			builder = builder.Owns(gatewayObject(gvk, "", ""))
		}
	}
	return builder.Named("simplecache").Complete(r)
}
