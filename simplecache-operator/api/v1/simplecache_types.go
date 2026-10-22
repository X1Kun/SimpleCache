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

package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SimpleCacheSpec describes the desired read-cache cluster.
type SimpleCacheSpec struct {
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +optional
	Size int32 `json:"size,omitempty"`
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`
	// SourceSecret selects PostgreSQL mode. The same-namespace Secret must
	// contain DATABASE_URL and CACHE_ADMIN_TOKEN. Empty retains the read-only demo.
	// Secret contents are consumed by Pods, never read by the controller.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	SourceSecret string `json:"sourceSecret,omitempty"`
	// +kubebuilder:default=67108864
	// +kubebuilder:validation:Minimum=1048576
	// +kubebuilder:validation:Maximum=134217728
	// +optional
	CacheBytes int64 `json:"cacheBytes,omitempty"`
	// HotCacheBytes reserves part of CacheBytes for remote read replicas.
	// Zero disables hot caching; replicas have at most a one-second local TTL.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=134217727
	// +optional
	HotCacheBytes int64 `json:"hotCacheBytes,omitempty"`
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	// +optional
	TTLSeconds int32 `json:"ttlSeconds,omitempty"`
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}
type SimpleCacheStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	ReadyReplicas      int32 `json:"readyReplicas,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=".spec.size"
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=".status.readyReplicas"
type SimpleCache struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SimpleCacheSpec   `json:"spec"`
	Status            SimpleCacheStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SimpleCacheList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SimpleCache `json:"items"`
}

func init() { SchemeBuilder.Register(&SimpleCache{}, &SimpleCacheList{}) }
