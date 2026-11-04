package discovery

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cluster"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func ptr[T any](v T) *T { return &v }
func slice(name, ip string, ready *bool) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "demo", Labels: map[string]string{discoveryv1.LabelServiceName: "cache-svc"}},
		AddressType: discoveryv1.AddressTypeIPv4, Ports: []discoveryv1.EndpointPort{{Name: ptr("cluster"), Port: ptr(int32(8001))}},
		Endpoints: []discoveryv1.Endpoint{{Addresses: []string{ip}, Conditions: discoveryv1.EndpointConditions{Ready: ready}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: name, Namespace: "demo"}}}}
}

func TestMultipleSlicesAndInvalidEndpointFields(t *testing.T) {
	a, b := slice("a", "10.0.0.1", ptr(true)), slice("b", "10.0.0.2", ptr(true))
	invalid := slice("invalid", "not-an-ip", ptr(true))
	foreign := slice("foreign", "10.0.0.3", ptr(true))
	foreign.Endpoints[0].TargetRef.Namespace = "other"
	wrongPort := slice("wrong-port", "10.0.0.4", ptr(true))
	wrongPort.Ports[0].Port = ptr(int32(0))
	ipv6 := slice("ipv6", "::1", ptr(true))
	ipv6.AddressType = discoveryv1.AddressTypeIPv6
	got := Members([]*discoveryv1.EndpointSlice{a, b, invalid, foreign, wrongPort, ipv6})
	if len(got) != 2 {
		t.Fatalf("unexpected usable members: %v", got)
	}
}

func TestWatchFailureRetainsLastValidMembers(t *testing.T) {
	client := fake.NewClientset(slice("a", "10.0.0.1", ptr(true)))
	client.PrependWatchReactor("endpointslices", func(ktesting.Action) (bool, watch.Interface, error) {
		return true, nil, errors.New("simulated watch interruption")
	})
	router := cluster.NewDirectory("entry", time.Second, "")
	defer router.Close()
	w, err := New(client, "demo", "cache-svc", router)
	if err != nil {
		t.Fatal(err)
	}
	var failures atomic.Int32
	w.ReportError = func() { failures.Add(1) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	deadline := time.After(3 * time.Second)
	for !w.Ready() || failures.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("watch failure was not observed after initial sync")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if members := router.Members(); len(members) != 1 || members[0].ID != "demo/a" {
		t.Fatalf("watch failure discarded members: %v", members)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher did not stop")
	}
	if w.Ready() {
		t.Fatal("stopped watcher remained ready")
	}
}
func TestMemberFiltering(t *testing.T) {
	a := slice("a", "10.0.0.1", ptr(true))
	b := slice("b", "10.0.0.2", ptr(false))
	c := slice("c", "10.0.0.3", nil)
	d := slice("d", "10.0.0.4", ptr(true))
	d.Endpoints[0].Conditions.Terminating = ptr(true)
	got := Members([]*discoveryv1.EndpointSlice{a, b, c, d})
	if len(got) != 2 || got[0].ID != "demo/a" || got[1].ID != "demo/c" {
		t.Fatal(got)
	}
}
func TestInformerAddDeleteAndAddressChange(t *testing.T) {
	client := fake.NewClientset(slice("a", "10.0.0.1", ptr(true)))
	router := cluster.NewDirectory("demo/a", time.Second, "")
	defer router.Close()
	watcher, err := New(client, "demo", "cache-svc", router)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watcher.Run(ctx)
	eventually := func(check func() bool) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			if check() {
				return
			}
			select {
			case <-deadline:
				t.Fatal("informer did not converge")
			case <-ticker.C:
			}
		}
	}
	eventually(func() bool { return watcher.Ready() && len(router.Members()) == 1 })
	updated := slice("a", "10.0.0.9", ptr(true))
	if _, err := client.DiscoveryV1().EndpointSlices("demo").Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(func() bool { m := router.Members(); return len(m) == 1 && m[0].Address == "10.0.0.9:8001" })
	if err := client.DiscoveryV1().EndpointSlices("demo").Delete(ctx, "a", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(func() bool { return len(router.Members()) == 0 && watcher.Ready() })
}
