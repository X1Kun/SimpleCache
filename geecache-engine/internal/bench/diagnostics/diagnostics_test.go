package diagnostics

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const metricFixture = `process_cpu_seconds_total 1.5
process_resident_memory_bytes 2048
go_memstats_heap_alloc_bytes 1024
simplecache_cache_bytes 512
simplecache_cache_removals_total 3
simplecache_source_loads_total{result="ok"} 7
simplecache_source_loads_total{result="error"} 1
`

func TestParseServerMetrics(t *testing.T) {
	m, err := parseMetrics(metricFixture)
	if err != nil {
		t.Fatal(err)
	}
	if m["source"] != 8 || m["cpu"] != 1.5 || m["rss"] != 2048 || m["removals"] != 3 {
		t.Fatalf("%v", m)
	}
	for _, body := range []string{"", strings.ReplaceAll(metricFixture, "1.5", "NaN"), strings.ReplaceAll(metricFixture, "1.5", "+Inf"), strings.ReplaceAll(metricFixture, "1.5", "-1")} {
		if _, err := parseMetrics(body); err == nil {
			t.Fatal("accepted incomplete/invalid metrics")
		}
	}
}

func TestOptionsRequireExplicitQuickMode(t *testing.T) {
	o := Options{Server: "server", Dir: "dir", Seconds: 10, Repeats: 3, Workers: 8}
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	o.Seconds = 1
	o.Repeats = 1
	if o.Validate() == nil {
		t.Fatal("short sample passed as baseline")
	}
	o.Quick = true
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	o.Workers = 33
	if o.Validate() == nil {
		t.Fatal("unbounded workers accepted")
	}
}

func TestReadValueDetectsIncorrectValues(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("key") {
		case "Auto-0":
			w.Write([]byte("Value-for-Auto-0"))
		case "Auto-1":
			w.Write([]byte("wrong"))
		default:
			http.Error(w, "failed", 503)
		}
	}))
	defer s.Close()
	c := newClient(1)
	defer c.CloseIdleConnections()
	for _, tc := range []struct {
		key        string
		bad, wrong bool
	}{{"Auto-0", false, false}, {"Auto-1", false, true}, {"Auto-2", true, false}} {
		bad, wrong := readValue(context.Background(), c, s.URL, tc.key, 0)
		if bad != tc.bad || wrong != tc.wrong {
			t.Fatalf("%s: %v %v", tc.key, bad, wrong)
		}
	}
}

func TestFailedStartupPreservesSummary(t *testing.T) {
	dir := t.TempDir()
	o := Options{Server: filepath.Join(dir, "missing-server"), Dir: dir, Seconds: 1, Repeats: 1, Workers: 1, Quick: true}
	summary, err := Run(context.Background(), o)
	if err == nil || summary.Status != "failed" || summary.Failure == "" {
		t.Fatalf("%+v %v", summary, err)
	}
	if _, err = os.Stat(filepath.Join(dir, "performance.json")); err != nil {
		t.Fatal(err)
	}
}

func TestCanceledRunIsNotSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	summary, err := Run(ctx, Options{Server: "unused", Dir: t.TempDir(), Seconds: 1, Repeats: 1, Workers: 1, Quick: true})
	if !errors.Is(err, context.Canceled) || summary.Status != "failed" {
		t.Fatalf("%+v %v", summary, err)
	}
}

func TestProfileFailureDoesNotWriteFakeProfile(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "disabled", 404) }))
	defer s.Close()
	path := filepath.Join(t.TempDir(), "cpu.pprof")
	if fetchProfile(context.Background(), s.URL, path) == nil {
		t.Fatal("accepted failed profile response")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("fake profile exists: %v", err)
	}
}
