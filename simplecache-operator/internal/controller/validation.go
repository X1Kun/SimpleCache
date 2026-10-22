package controller

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	cachev1 "x1kun.com/simplecache-operator/api/v1"
)

func validateSpec(cr *cachev1.SimpleCache) error {
	if cr.Spec.SourceSecret != "" && len(validation.IsDNS1123Subdomain(cr.Spec.SourceSecret)) != 0 {
		return fmt.Errorf("sourceSecret must be a valid Secret name")
	}
	if strings.TrimSpace(cr.Spec.Image) == "" || cr.Spec.Size < 1 || cr.Spec.Size > 10 || cr.Spec.CacheBytes < 1<<20 || cr.Spec.CacheBytes > 128<<20 || cr.Spec.TTLSeconds < 1 || cr.Spec.TTLSeconds > 3600 {
		return fmt.Errorf("image, replica count, cache capacity or TTL is invalid")
	}
	if cr.Spec.HotCacheBytes < 0 || cr.Spec.HotCacheBytes >= cr.Spec.CacheBytes {
		return fmt.Errorf("hotCacheBytes must be nonnegative and below cacheBytes")
	}
	for name, q := range cr.Spec.Resources.Requests {
		if q.Sign() < 0 {
			return fmt.Errorf("negative request for %s", name)
		}
		if limit, ok := cr.Spec.Resources.Limits[name]; ok && q.Cmp(limit) > 0 {
			return fmt.Errorf("request exceeds limit for %s", name)
		}
	}
	for name, q := range cr.Spec.Resources.Limits {
		if q.Sign() < 0 {
			return fmt.Errorf("negative limit for %s", name)
		}
	}
	return nil
}

func resourceDefaults(input corev1.ResourceRequirements) corev1.ResourceRequirements {
	result := *input.DeepCopy()
	if result.Requests == nil {
		result.Requests = corev1.ResourceList{}
	}
	if result.Limits == nil {
		result.Limits = corev1.ResourceList{}
	}
	requests := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")}
	for name, q := range requests {
		if _, ok := result.Requests[name]; ok {
			continue
		}
		if limit, ok := result.Limits[name]; ok && limit.Cmp(q) < 0 {
			q = limit.DeepCopy()
		}
		result.Requests[name] = q
	}
	if _, ok := result.Limits[corev1.ResourceMemory]; !ok {
		limit := resource.MustParse("256Mi")
		if request := result.Requests[corev1.ResourceMemory]; request.Cmp(limit) > 0 {
			limit = request.DeepCopy()
		}
		result.Limits[corev1.ResourceMemory] = limit
	}
	return result
}
