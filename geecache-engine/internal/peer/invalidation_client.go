package peer

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
)

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
