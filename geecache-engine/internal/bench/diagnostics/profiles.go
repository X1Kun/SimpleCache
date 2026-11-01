package diagnostics

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

func fetchProfile(ctx context.Context, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	// Explicit transport avoids environment proxies for loopback diagnostic traffic.
	client := newClient(4)
	client.Timeout = 0
	defer client.CloseIdleConnections()
	r, err := client.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return fmt.Errorf("profile HTTP %d: %s", r.StatusCode, url)
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, (64<<20)+1))
	if err != nil {
		return err
	}
	if len(b) == 0 || len(b) > 64<<20 {
		return fmt.Errorf("invalid profile size %d", len(b))
	}
	return os.WriteFile(path, b, 0644)
}

func captureProfiles(ctx context.Context, origin, dir string, seconds int) <-chan error {
	done := make(chan error, 1)
	go func() {
		var wg sync.WaitGroup
		failures := make(chan error, 4)
		for file, endpoint := range map[string]string{"cpu": "profile", "allocs": "allocs", "mutex": "mutex", "block": "block"} {
			wg.Add(1)
			go func(file, endpoint string) {
				defer wg.Done()
				if err := fetchProfile(ctx, fmt.Sprintf("%s/debug/pprof/%s?seconds=%d", origin, endpoint, seconds), filepath.Join(dir, file+".pprof")); err != nil {
					failures <- err
				}
			}(file, endpoint)
		}
		wg.Wait()
		close(failures)
		for err := range failures {
			done <- err
			return
		}
		done <- nil
	}()
	return done
}
