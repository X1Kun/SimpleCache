package peer

import (
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/peer/hashring"
)

type Member struct {
	ID      string
	Address string
}
type snapshot struct {
	ring    *hashring.Map
	clients map[string]*Client
	members []Member
}
type Router struct {
	selfID   string
	client   *http.Client
	timeout  time.Duration
	token    string
	updateMu sync.Mutex
	state    atomic.Pointer[snapshot]
}

func NewRouter(selfID string, timeout time.Duration) *Router {
	return NewRouterWithToken(selfID, timeout, "")
}

func NewRouterWithToken(selfID string, timeout time.Duration, token string) *Router {
	if selfID == "" || timeout <= 0 {
		panic("invalid router identity or timeout")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 128
	transport.MaxIdleConnsPerHost = 32
	r := &Router{selfID: selfID, timeout: timeout, token: token, client: &http.Client{
		Transport: transport, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	_ = r.Update(nil)
	return r
}
func (r *Router) Update(input []Member) error {
	r.updateMu.Lock()
	defer r.updateMu.Unlock()
	members := append([]Member(nil), input...)
	sort.Slice(members, func(i, j int) bool {
		if members[i].ID != members[j].ID {
			return members[i].ID < members[j].ID
		}
		return members[i].Address < members[j].Address
	})
	normalized := make([]Member, 0, len(members))
	for _, m := range members {
		if m.ID == "" || !validAddress(m.Address) {
			return fmt.Errorf("invalid member %q: %q", m.ID, m.Address)
		}
		if len(normalized) > 0 && normalized[len(normalized)-1].ID == m.ID {
			// Lexical address order is the deterministic tie-break for transient duplicates.
			continue
		}
		normalized = append(normalized, m)
	}
	previous := r.state.Load()
	if previous != nil && sameMembers(previous.members, normalized) {
		return nil
	}
	next := &snapshot{ring: hashring.New(100, nil), clients: make(map[string]*Client), members: normalized}
	for _, m := range normalized {
		next.ring.Add(m.ID)
		next.clients[m.ID] = &Client{address: m.Address, http: r.client, timeout: r.timeout, token: r.token}
	}
	r.state.Store(next)
	return nil
}
func validAddress(address string) bool {
	if strings.ContainsAny(address, "/?#") {
		return false
	}
	host, port, err := net.SplitHostPort(address)
	number, parseErr := strconv.Atoi(port)
	return err == nil && parseErr == nil && host != "" && number > 0 && number <= 65535
}
func sameMembers(a, b []Member) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func (r *Router) PickPeer(key string) (cache.PeerGetter, bool) {
	s := r.state.Load()
	id := s.ring.Get(key)
	if id == "" || id == r.selfID {
		return nil, false
	}
	return s.clients[id], true
}
func (r *Router) Members() []Member { return append([]Member(nil), r.state.Load().members...) }
func (r *Router) Close()            { r.client.CloseIdleConnections() }
