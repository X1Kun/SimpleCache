package cache

const DefaultMissWaiters = 128

// AdmissionStats are instantaneous channel occupancies, not an RSS limit.
// API and Peer miss waiters have separate lanes; hits never acquire them.
type AdmissionStats struct {
	APIWaiters    int
	PeerWaiters   int
	RouteTasks    int
	SourcePending int
}

func (g *Group) AdmissionStats() AdmissionStats {
	return AdmissionStats{APIWaiters: len(g.apiWaiters), PeerWaiters: len(g.peerWaiters), RouteTasks: len(g.routeTasks), SourcePending: len(g.opts.Limiter.waiting)}
}

func (g *Group) admit(slots chan struct{}, kind string) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		g.observe("rejected", kind, 0)
		return false
	}
}
