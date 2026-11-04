package cluster

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
)

type Member struct {
	ID      string
	Address string
}
type snapshot struct {
	clients map[string]*Client
	members []Member
}
type Directory struct {
	selfID   string
	client   *http.Client
	timeout  time.Duration
	token    string
	updateMu sync.Mutex
	state    atomic.Pointer[snapshot]
}

func NewDirectory(selfID string, timeout time.Duration, token string) *Directory {
	if selfID == "" || timeout <= 0 {
		panic("invalid directory identity or timeout")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 128
	transport.MaxIdleConnsPerHost = 32
	r := &Directory{selfID: selfID, timeout: timeout, token: token, client: &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	_ = r.Update(nil)
	return r
}
func (r *Directory) Update(input []Member) error {
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
	next := &snapshot{clients: make(map[string]*Client), members: normalized}
	for _, m := range normalized {
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
func (r *Directory) Members() []Member { return append([]Member(nil), r.state.Load().members...) }
func (r *Directory) Close()            { r.client.CloseIdleConnections() }
