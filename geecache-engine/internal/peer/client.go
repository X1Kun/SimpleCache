package peer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	pb "github.com/X1Kun/simpleCache/geecache-engine/internal/peer/peerpb"
	"google.golang.org/protobuf/proto"
)

const (
	BasePath      = "/_geecache/"
	MaxBodyBytes  = cache.MaxValueBytes + 1024
	ErrorHeader   = "X-SimpleCache-Error"
	TTLHeader     = "X-SimpleCache-TTL-Nanoseconds"
	VersionHeader = "X-SimpleCache-Version"
)

var ErrGroupNotFound = errors.New("peer group not found")

type Client struct {
	address string
	http    *http.Client
	timeout time.Duration
	token   string
}

func (c *Client) Get(ctx context.Context, group, key string) ([]byte, error) {
	value, err := c.GetWithExpiry(ctx, group, key)
	return value.Value, err
}

func (c *Client) GetWithExpiry(ctx context.Context, group, key string) (cache.PeerValue, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	target := "http://" + c.address + BasePath + url.PathEscape(group) + "/" + url.PathEscape(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return cache.PeerValue{}, err
	}
	start := time.Now()
	response, err := c.http.Do(req)
	if err != nil {
		return cache.PeerValue{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusNotFound {
			switch response.Header.Get(ErrorHeader) {
			case "KEY_NOT_FOUND":
				return cache.PeerValue{}, cache.ErrNotFound
			case "GROUP_NOT_FOUND":
				return cache.PeerValue{}, ErrGroupNotFound
			}
		}
		return cache.PeerValue{}, fmt.Errorf("peer returned %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxBodyBytes+1))
	if err != nil {
		return cache.PeerValue{}, err
	}
	if len(body) > MaxBodyBytes {
		return cache.PeerValue{}, errors.New("peer body exceeds limit")
	}
	var result pb.Response
	if err := proto.Unmarshal(body, &result); err != nil {
		return cache.PeerValue{}, fmt.Errorf("decode peer: %w", err)
	}
	if len(result.Value) > cache.MaxValueBytes {
		return cache.PeerValue{}, cache.ErrValueTooLarge
	}
	value := cache.PeerValue{Value: result.Value}
	if raw := response.Header.Get(VersionHeader); raw != "" {
		version, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || version <= 0 || strconv.FormatInt(version, 10) != raw {
			return cache.PeerValue{}, errors.New("invalid peer source version")
		}
		value.Version = version
	}
	if raw := response.Header.Get(TTLHeader); raw != "" {
		nanos, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || nanos < 0 {
			return cache.PeerValue{}, errors.New("invalid peer TTL")
		}
		// Subtract the entire request lifetime, conservatively including server
		// work. Relative TTL does not depend on synchronized wall clocks.
		deadline := start.Add(time.Duration(nanos))
		if deadline.After(time.Now()) {
			value.ExpiresAt = deadline
		}
	}
	return value, nil
}
