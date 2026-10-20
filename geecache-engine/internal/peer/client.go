package peer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	pb "github.com/X1Kun/simpleCache/geecache-engine/internal/peer/peerpb"
	"google.golang.org/protobuf/proto"
)

const (
	BasePath     = "/_geecache/"
	MaxBodyBytes = cache.MaxValueBytes + 1024
	ErrorHeader  = "X-SimpleCache-Error"
)

var ErrGroupNotFound = errors.New("peer group not found")

type Client struct {
	address string
	http    *http.Client
	timeout time.Duration
	token   string
}

func (c *Client) Get(ctx context.Context, group, key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	target := "http://" + c.address + BasePath + url.PathEscape(group) + "/" + url.PathEscape(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusNotFound {
			switch response.Header.Get(ErrorHeader) {
			case "KEY_NOT_FOUND":
				return nil, cache.ErrNotFound
			case "GROUP_NOT_FOUND":
				return nil, ErrGroupNotFound
			}
		}
		return nil, fmt.Errorf("peer returned %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxBodyBytes {
		return nil, errors.New("peer body exceeds limit")
	}
	var result pb.Response
	if err := proto.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode peer: %w", err)
	}
	if len(result.Value) > cache.MaxValueBytes {
		return nil, cache.ErrValueTooLarge
	}
	return result.Value, nil
}
