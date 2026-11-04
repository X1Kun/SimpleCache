package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/demo"
)

func TestRunRetainsContinuityEvidence(t *testing.T) {
	for _, mode := range []string{"correct", "wrong", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				switch mode {
				case "wrong":
					fmt.Fprint(w, "wrong")
				case "unavailable":
					w.WriteHeader(503)
				default:
					fmt.Fprint(w, demo.AutoValue(r.URL.Query().Get("key"), 0))
				}
				if requests == 12 {
					if err := os.WriteFile(filepath.Join(dir, "probe.stop"), nil, 0600); err != nil {
						t.Error(err)
					}
				}
			}))
			defer server.Close()
			got, err := Run(context.Background(), Options{Origin: server.URL, Dir: dir, Workers: 1, Duration: 2 * time.Second, Interval: time.Millisecond})
			if (err == nil) != (mode == "correct") || got.Requests != 12 || !got.StopSeen {
				t.Fatalf("summary=%+v err=%v", got, err)
			}
			if mode == "wrong" && (got.Incorrect != 12 || got.Errors != 0) {
				t.Fatal(got)
			}
			if mode == "unavailable" && (got.Errors != 12 || got.Incorrect != 0) {
				t.Fatal(got)
			}
			if mode != "correct" && len(got.FailureExamples) != 10 {
				t.Fatal("missing evidence limit")
			}
			data, err := os.ReadFile(filepath.Join(dir, "probe.json"))
			if err != nil {
				t.Fatal(err)
			}
			var saved Summary
			if err = json.Unmarshal(data, &saved); err != nil || saved.Status != got.Status || saved.Requests != 12 {
				t.Fatalf("saved=%+v err=%v", saved, err)
			}
		})
	}
}

func TestRunIncompleteOrCanceledIsFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, demo.AutoValue(r.URL.Query().Get("key"), 0))
	}))
	defer server.Close()
	for _, mode := range []string{"deadline", "canceled", "premature-stop"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			if mode == "premature-stop" {
				if err := os.WriteFile(filepath.Join(dir, "probe.stop"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := Run(ctx, Options{Origin: server.URL, Dir: dir, Workers: 1, Duration: 20 * time.Millisecond, Interval: time.Millisecond})
			if err == nil || got.Status != "failed" {
				t.Fatalf("summary=%+v err=%v", got, err)
			}
			if _, err = os.Stat(filepath.Join(dir, "probe.json")); err != nil {
				t.Fatal("failed run lost evidence", err)
			}
			if mode == "premature-stop" && (!got.StopSeen || got.Requests != 0) {
				t.Fatal(got)
			}
		})
	}
}

func TestRunInvalidOptionsDoNotCreateArtifacts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-created")
	for _, o := range []Options{{Origin: "http://localhost/api", Dir: dir}, {Origin: "http://localhost", Dir: dir, Workers: -1}, {Origin: "http://localhost", Dir: dir, Duration: -time.Second}, {Origin: "http://localhost", Dir: dir, Interval: 2 * time.Second}} {
		if _, err := Run(context.Background(), o); err == nil {
			t.Fatal("invalid options accepted")
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("invalid options created artifacts")
	}
}
