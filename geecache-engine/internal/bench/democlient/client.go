// Package democlient checks the synthetic Auto-* dataset over the public API.
// It is shared by continuity probes and local diagnostics, not business clients.
package democlient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/demo"
)

const maxBodyBytes = 1 << 20

type Result struct {
	Failed    bool
	Incorrect bool
	Status    int
	ElapsedMS float64
	Detail    string
}

func ValidateOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || strings.Contains(origin, "#") {
		return fmt.Errorf("require an HTTP(S) origin without credentials, path, query or fragment")
	}
	return nil
}

func NewClient(workers int, timeout time.Duration) *http.Client {
	if workers < 1 || timeout <= 0 {
		panic("invalid demo client budget")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxIdleConns = workers + 4
	transport.MaxIdleConnsPerHost = workers + 4
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func Read(ctx context.Context, client *http.Client, origin, key string, valueBytes int) (result Result) {
	start := time.Now()
	defer func() { result.ElapsedMS = float64(time.Since(start)) / float64(time.Millisecond) }()
	if err := ValidateOrigin(origin); err != nil {
		result.Failed = true
		result.Detail = err.Error()
		return
	}
	target := strings.TrimSuffix(origin, "/") + "/api?key=" + strings.ReplaceAll(url.QueryEscape(key), "+", "%20")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		result.Failed = true
		result.Detail = "request construction failed"
		return
	}
	response, err := client.Do(request)
	if err != nil {
		result.Failed = true
		result.Detail = "transport or deadline failure"
		return
	}
	defer response.Body.Close()
	result.Status = response.StatusCode
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	result.Failed = err != nil || response.StatusCode != http.StatusOK || len(body) > maxBodyBytes
	result.Incorrect = !result.Failed && string(body) != demo.AutoValue(key, valueBytes)
	if result.Failed || result.Incorrect {
		excerpt := body
		if len(excerpt) > 256 {
			excerpt = excerpt[:256]
		}
		result.Detail = fmt.Sprintf("key=%q status=%d body_prefix=%q read_failed=%v oversized=%v", key, response.StatusCode, excerpt, err != nil, len(body) > maxBodyBytes)
	}
	return
}
