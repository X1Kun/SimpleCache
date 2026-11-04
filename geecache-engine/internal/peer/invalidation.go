package peer

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
)

type InvalidationResult struct {
	Node   string `json:"node"`
	Status string `json:"status"`
}

type InvalidationReport struct {
	// Captured directory size, for relay readiness; not a wire delivery promise.
	KnownMembers int                  `json:"-"`
	Version      int64                `json:"version,omitempty"`
	Scope        string               `json:"scope"`
	Complete     bool                 `json:"complete"`
	Results      []InvalidationResult `json:"results"`
}

// Invalidate uses one immutable membership snapshot. The initiating node is
// always invalidated, even when it is absent from discovery. Remote handlers
// perform local invalidation only. This is best effort, not reliable messaging.
func (r *Router) Invalidate(ctx context.Context, group *cache.Group, key string) (InvalidationReport, error) {
	return r.InvalidateVersion(ctx, group, key, 0)
}

func (r *Router) InvalidateVersion(ctx context.Context, group *cache.Group, key string, version int64) (InvalidationReport, error) {
	if err := group.InvalidateVersion(key, version); err != nil {
		return InvalidationReport{}, err
	}
	snapshot := r.state.Load()
	report := InvalidationReport{
		Scope: "known_members_snapshot", Complete: true, Version: version, KnownMembers: len(snapshot.members),
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
				if err := clients[i].InvalidateVersion(ctx, group.Name(), key, version); err != nil {
					status = "failed"
					if errors.Is(err, ErrUnversionedInvalidation) {
						status = "unversioned"
					}
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

const InvalidationTimeout = time.Second

func (c *Client) Invalidate(ctx context.Context, group, key string) error {
	return c.InvalidateVersion(ctx, group, key, 0)
}

func (c *Client) InvalidateVersion(ctx context.Context, group, key string, version int64) error {
	if version < 0 {
		return cache.ErrInvalidVersion
	}
	ctx, cancel := context.WithTimeout(ctx, InvalidationTimeout)
	defer cancel()
	target := "http://" + c.address + BasePath + url.PathEscape(group) + "/" + url.PathEscape(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if version > 0 {
		req.Header.Set(InvalidationVersionHeader, strconv.FormatInt(version, 10))
	}
	response, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return errors.New("peer did not acknowledge invalidation")
	}
	if version > 0 {
		ack, err := ParseInvalidationVersion(response.Header)
		if err != nil || ack != version {
			return ErrUnversionedInvalidation
		}
	}
	return nil
}
