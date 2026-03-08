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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// SimpleCacheSpec defines the desired state of SimpleCache
type SimpleCacheSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// foo is an example field of SimpleCache. Edit simplecache_types.go to remove/update
	// +optional
	Foo *string `json:"foo,omitempty"`

	// Size 定义期望的缓存节点数量
	// +kubebuilder:validation:Minimum=1
	Size int32 `json:"size"`
	// Image 定义拉起缓存节点所使用的 Docker 镜像 (比如: "x1kun/geecache:v1")
	Image string `json:"image"`
}

// SimpleCacheStatus defines the observed state of SimpleCache.
type SimpleCacheStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the SimpleCache resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// SimpleCache is the Schema for the simplecaches API
type SimpleCache struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SimpleCache
	// +required
	Spec SimpleCacheSpec `json:"spec"`

	// status defines the observed state of SimpleCache
	// +optional
	Status SimpleCacheStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SimpleCacheList contains a list of SimpleCache
type SimpleCacheList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SimpleCache `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SimpleCache{}, &SimpleCacheList{})
}
