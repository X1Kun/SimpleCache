package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"strconv"
	"sync/atomic"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cluster"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	informers "k8s.io/client-go/informers/discovery/v1"
	"k8s.io/client-go/kubernetes"
	kcache "k8s.io/client-go/tools/cache"
)

type Watcher struct {
	informer    kcache.SharedIndexInformer
	router      *cluster.Directory
	ready       atomic.Bool
	changes     chan struct{}
	Report      func(count int, changed bool)
	ReportError func()
}

func New(client kubernetes.Interface, namespace, service string, router *cluster.Directory) (*Watcher, error) {
	if namespace == "" || service == "" {
		return nil, fmt.Errorf("namespace and member service are required")
	}
	informer := informers.NewFilteredEndpointSliceInformer(client, namespace, 0, kcache.Indexers{}, func(opts *metav1.ListOptions) {
		opts.LabelSelector = discoveryv1.LabelServiceName + "=" + service
	})
	w := &Watcher{informer: informer, router: router, changes: make(chan struct{}, 1)}
	if err := informer.SetWatchErrorHandler(func(_ *kcache.Reflector, err error) {
		if w.ReportError != nil {
			w.ReportError()
		}
		slog.Error("Discovery list/watch failed", "error", err)
	}); err != nil {
		return nil, err
	}
	signal := func() {
		select {
		case w.changes <- struct{}{}:
		default:
		}
	}
	_, err := informer.AddEventHandler(kcache.ResourceEventHandlerFuncs{
		AddFunc: func(any) { signal() }, UpdateFunc: func(any, any) { signal() }, DeleteFunc: func(any) { signal() },
	})
	return w, err
}
func (w *Watcher) Ready() bool { return w.ready.Load() }
func (w *Watcher) Run(ctx context.Context) {
	go w.informer.Run(ctx.Done())
	if !kcache.WaitForCacheSync(ctx.Done(), w.informer.HasSynced) {
		return
	}
	if w.rebuild() {
		w.ready.Store(true)
	}
	for {
		select {
		case <-ctx.Done():
			w.ready.Store(false)
			return
		case <-w.changes:
			if w.rebuild() {
				w.ready.Store(true)
			}
		}
	}
}
func (w *Watcher) rebuild() bool {
	var slices []*discoveryv1.EndpointSlice
	for _, obj := range w.informer.GetStore().List() {
		if s, ok := obj.(*discoveryv1.EndpointSlice); ok {
			slices = append(slices, s)
		}
	}
	before := w.router.Members()
	if err := w.router.Update(Members(slices)); err != nil {
		slog.Error("Could not publish members", "error", err)
		return false
	}
	after := w.router.Members()
	if w.Report != nil {
		w.Report(len(after), !reflect.DeepEqual(before, after))
	}
	return true
}

// Members selects usable IPv4 Pod endpoints. The informer already restricts Service and namespace.
func Members(slices []*discoveryv1.EndpointSlice) []cluster.Member {
	var result []cluster.Member
	for _, s := range slices {
		if s.AddressType != discoveryv1.AddressTypeIPv4 {
			continue
		}
		var port int32
		for _, p := range s.Ports {
			if p.Name != nil && *p.Name == "cluster" && p.Port != nil && (p.Protocol == nil || *p.Protocol == corev1.ProtocolTCP) {
				port = *p.Port
				break
			}
		}
		if port < 1 || port > 65535 {
			continue
		}
		for _, e := range s.Endpoints {
			if e.Conditions.Ready != nil && !*e.Conditions.Ready {
				continue
			}
			if e.Conditions.Terminating != nil && *e.Conditions.Terminating {
				continue
			}
			if e.TargetRef == nil || e.TargetRef.Kind != "Pod" || e.TargetRef.Name == "" {
				continue
			}
			ns := e.TargetRef.Namespace
			if ns == "" {
				ns = s.Namespace
			}
			if ns == "" || ns != s.Namespace {
				continue
			}
			for _, address := range e.Addresses {
				ip := net.ParseIP(address)
				if ip == nil || ip.To4() == nil {
					continue
				}
				result = append(result, cluster.Member{ID: ns + "/" + e.TargetRef.Name, Address: net.JoinHostPort(ip.String(), strconv.Itoa(int(port)))})
			}
		}
	}
	return result
}
