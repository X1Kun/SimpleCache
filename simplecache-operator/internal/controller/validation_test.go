package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	cachev1 "x1kun.com/simplecache-operator/api/v1"
)

func TestDerivedResourceNameBounds(t *testing.T) {
	for _, gateway := range []bool{false, true} {
		limit := 59
		if gateway {
			limit = 55
		}
		for _, tc := range []struct {
			name  string
			valid bool
		}{{"cache", true}, {strings.Repeat("a", limit), true}, {strings.Repeat("a", limit+1), false}, {"review.cache", false}, {"123cache", false}, {"cache-", false}} {
			cr := &cachev1.SimpleCache{ObjectMeta: metav1.ObjectMeta{Name: tc.name}, Spec: cachev1.SimpleCacheSpec{Image: "cache:test"}}
			if gateway {
				cr.Spec.Gateway = &cachev1.GatewaySpec{}
			}
			defaults(cr)
			if err := validateSpec(cr); (err == nil) != tc.valid {
				t.Errorf("gateway=%v name=%q valid=%v err=%v", gateway, tc.name, tc.valid, err)
			}
		}
	}
}
