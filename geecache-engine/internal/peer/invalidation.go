package peer

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
)

const InvalidationTimeout = time.Second

// Authorized disables mutations entirely when no token has been configured.
func Authorized(r *http.Request, token string) bool {
	return token != "" && subtle.ConstantTimeCompare(
		[]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) == 1
}

type InvalidationResult struct {
	Node   string `json:"node"`
	Status string `json:"status"`
}

type InvalidationReport struct {
	Scope    string               `json:"scope"`
	Complete bool                 `json:"complete"`
	Results  []InvalidationResult `json:"results"`
}

func (c *Client) Invalidate(ctx context.Context, group, key string) error {
	ctx, cancel := context.WithTimeout(ctx, InvalidationTimeout)
	defer cancel()
	target := "http://" + c.address + BasePath + url.PathEscape(group) + "/" + url.PathEscape(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return errors.New("peer did not acknowledge invalidation")
	}
	return nil
}

// Invalidate uses one immutable membership snapshot. The initiating node is
// always invalidated, even when it is absent from discovery. Remote handlers
// perform local invalidation only. This is best effort, not reliable messaging.
func (r *Router) Invalidate(ctx context.Context, group *cache.Group, key string) (InvalidationReport, error) {
	if err := group.Invalidate(key); err != nil {
		return InvalidationReport{}, err
	}
	snapshot := r.state.Load()
	report := InvalidationReport{
		Scope: "known_members_snapshot", Complete: true,
		Results: []InvalidationResult{{Node: r.selfID, Status: "ok"}},
	}
	var clients []*Client
	for _, member := range snapshot.members {
		if member.ID != r.selfID {
			report.Results = append(report.Results, InvalidationResult{Node: member.ID})
			clients = append(clients, snapshot.clients[member.ID])
		}
	}
	ctx, cancel := context.WithTimeout(ctx, InvalidationTimeout)
	defer cancel()
	// A fixed worker count bounds fan-out for arbitrary static memberships.
	jobs := make(chan int)
	var workers sync.WaitGroup
	for n := 0; n < min(8, len(clients)); n++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				status := "ok"
				if err := clients[i].Invalidate(ctx, group.Name(), key); err != nil {
					status = "failed"
					if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
						status = "timeout"
					}
				}
				report.Results[i+1].Status = status
			}
		}()
	}
	for i := range clients {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
	for _, result := range report.Results {
		if result.Status != "ok" {
			report.Complete = false
		}
	}
	return report, nil
}
