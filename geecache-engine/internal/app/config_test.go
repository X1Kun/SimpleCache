package app

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/cluster"
)

func TestConfigRejectsIncompleteKubernetesDiscovery(t *testing.T) {
	t.Setenv("DISCOVERY_MODE", "kubernetes")
	t.Setenv("POD_NAME", "")
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("CLUSTER_SERVICE", "")
	if _, err := FromEnv(8001, true); err == nil {
		t.Fatal("incomplete Kubernetes identity accepted")
	}
	t.Setenv("DISCOVERY_MODE", "static")
	t.Setenv("SELF_ADDR", "")
	t.Setenv("MEMBERS", "")
	c, err := FromEnv(8001, true)
	if err != nil || len(c.Members) != 1 || c.Members[0].ID != c.SelfID {
		t.Fatal(c, err)
	}
	t.Setenv("SELF_ADDR", "http://user:secret@localhost:8001")
	if _, err := FromEnv(8001, true); err == nil {
		t.Fatal("credential URL accepted")
	}
}

func TestKubernetesIdentityIsIndependentOfTransport(t *testing.T) {
	t.Setenv("DISCOVERY_MODE", "kubernetes")
	t.Setenv("POD_NAME", "cache-1")
	t.Setenv("POD_NAMESPACE", "demo")
	t.Setenv("CLUSTER_SERVICE", "cache-svc")
	t.Setenv("SELF_ADDR", "http://stale-address:8001")
	t.Setenv("MEMBERS", "http://stale-address:8001")
	c, err := FromEnv(8001, true)
	if err != nil || c.SelfID != "demo/cache-1" || len(c.Members) != 0 {
		t.Fatalf("unexpected Kubernetes configuration: %+v %v", c, err)
	}
	t.Setenv("DISCOVERY_MODE", "unknown")
	if _, err := FromEnv(8001, true); err == nil {
		t.Fatal("unknown discovery mode accepted")
	}
}

func TestAdmissionConfigurationBoundsAndZeroSourceQueue(t *testing.T) {
	t.Setenv("SOURCE_MODE", "demo")
	t.Setenv("DISCOVERY_MODE", "static")
	for _, name := range []string{"SOURCE_MAX_PENDING", "MAX_MISS_WAITERS", "WRITE_MAX_CONCURRENCY"} {
		t.Setenv(name, "")
	}
	c, err := FromEnv(8001, true)
	if err != nil || c.SourceMaxPending != cache.DefaultSourcePending || c.MaxMissWaiters != cache.DefaultMissWaiters || c.WriteConcurrency != DefaultWriteConcurrency {
		t.Fatal(c, err)
	}
	t.Setenv("SOURCE_MAX_PENDING", "0")
	c, err = FromEnv(8001, true)
	if err != nil || c.SourceMaxPending != 0 {
		t.Fatal("zero queue was defaulted away", c.SourceMaxPending, err)
	}
	for _, tc := range []struct{ name, value string }{
		{"SOURCE_MAX_PENDING", "-1"}, {"SOURCE_MAX_PENDING", "4097"},
		{"MAX_MISS_WAITERS", "0"}, {"MAX_MISS_WAITERS", "4097"},
		{"WRITE_MAX_CONCURRENCY", "0"}, {"WRITE_MAX_CONCURRENCY", "257"},
		{"WRITE_MAX_CONCURRENCY", "invalid"},
	} {
		t.Run(tc.name+"-"+tc.value, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)
			if _, err := FromEnv(8001, true); err == nil {
				t.Fatal("invalid admission accepted", tc)
			}
		})
	}
}

// Programmatic configurations may request ephemeral listeners and retain the
// existing component defaults without borrowing any process environment.
func runtimeTestConfig() Config {
	return Config{ClusterAddr: "127.0.0.1:0", APIAddr: "127.0.0.1:0", API: true,
		SelfID: "injected", DiscoveryMode: "static", CacheBytes: 1 << 20, SourceConcurrency: 1}
}

func TestConfigValidateRangesCombinationsAndPurity(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Config)
		valid  bool
	}{
		{"defaults", func(*Config) {}, true},
		{"upper-bounds", func(c *Config) {
			c.CacheBytes = 128 << 20
			c.TTL = time.Hour
			c.SourceConcurrency = 1024
			c.SourceMaxPending = 4096
			c.MaxMissWaiters = 4096
			c.WriteConcurrency = 256
		}, true},
		{"zero-queue", func(c *Config) { c.SourceMaxPending = 0 }, true},
		{"short-programmatic-ttl", func(c *Config) { c.TTL = time.Millisecond }, true},
		{"disabled-api", func(c *Config) { c.API = false; c.APIAddr = "" }, true},
		{"debug", func(c *Config) { c.DebugAddr = "127.0.0.1:0"; c.ProfileContention = true }, true},
		{"injected-postgres", func(c *Config) { c.SourceMode = "postgres" }, true},
		{"kubernetes", func(c *Config) {
			c.DiscoveryMode = "kubernetes"
			c.Namespace = "demo"
			c.ClusterService = "cache"
			c.SelfID = "demo/cache-0"
		}, true},
		{"no-peer-address", func(c *Config) { c.ClusterAddr = "" }, false},
		{"invalid-peer-port", func(c *Config) { c.ClusterAddr = "localhost:65536" }, false},
		{"invalid-api-port", func(c *Config) { c.APIAddr = "localhost:not-a-port" }, false},
		{"enabled-api-missing-address", func(c *Config) { c.APIAddr = "" }, false},
		{"no-self-id", func(c *Config) { c.SelfID = "" }, false},
		{"zero-cache", func(c *Config) { c.CacheBytes = 0 }, false},
		{"cache-upper", func(c *Config) { c.CacheBytes = 129 << 20 }, false},
		{"negative-ttl", func(c *Config) { c.TTL = -1 }, false},
		{"ttl-upper", func(c *Config) { c.TTL = time.Hour + 1 }, false},
		{"zero-source", func(c *Config) { c.SourceConcurrency = 0 }, false},
		{"source-upper", func(c *Config) { c.SourceConcurrency = 1025 }, false},
		{"negative-queue", func(c *Config) { c.SourceMaxPending = -1 }, false},
		{"queue-upper", func(c *Config) { c.SourceMaxPending = 4097 }, false},
		{"negative-waiters", func(c *Config) { c.MaxMissWaiters = -1 }, false},
		{"waiters-upper", func(c *Config) { c.MaxMissWaiters = 4097 }, false},
		{"negative-writes", func(c *Config) { c.WriteConcurrency = -1 }, false},
		{"writes-upper", func(c *Config) { c.WriteConcurrency = 257 }, false},
		{"injected-custom-source", func(c *Config) { c.SourceMode = "custom" }, true},
		{"unsupported-discovery", func(c *Config) { c.DiscoveryMode = "unknown" }, false},
		{"missing-kubernetes-fields", func(c *Config) { c.DiscoveryMode = "kubernetes" }, false},
		{"mismatched-kubernetes-id", func(c *Config) {
			c.DiscoveryMode = "kubernetes"
			c.Namespace = "demo"
			c.ClusterService = "cache"
			c.SelfID = "other/cache-0"
		}, false},
		{"unsafe-debug", func(c *Config) { c.DebugAddr = "0.0.0.0:6060" }, false},
		{"profile-without-debug", func(c *Config) { c.ProfileContention = true }, false},
		{"bad-member-id", func(c *Config) { c.Members = []cluster.Member{{Address: "localhost:8001"}} }, false},
		{"bad-member-address", func(c *Config) { c.Members = []cluster.Member{{ID: "node", Address: "localhost:0"}} }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := runtimeTestConfig()
			tc.change(&c)
			before := c
			err := c.Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if !reflect.DeepEqual(c, before) {
				t.Fatal("Validate mutated configuration")
			}
			if !tc.valid {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				source := cache.GetterFunc(func(context.Context, string) ([]byte, error) {
					t.Error("invalid config reached source")
					return nil, nil
				})
				if e := Run(ctx, c); e == nil {
					t.Fatal("Run bypassed validation")
				}
				if e := RunWithSource(ctx, c, source); e == nil {
					t.Fatal("RunWithSource bypassed validation")
				}
			}
		})
	}
}

func clearRuntimeEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"DISCOVERY_MODE", "POD_NAME", "POD_NAMESPACE", "CLUSTER_SERVICE", "READ_ROUTING_MODE", "SOURCE_MODE", "DATABASE_URL", "CACHE_ADMIN_TOKEN", "API_ADDR", "DEBUG_ADDR", "CACHE_BYTES", "HOT_CACHE_BYTES", "TTL_SECONDS", "SOURCE_MAX_CONCURRENCY", "SOURCE_MAX_PENDING", "MAX_MISS_WAITERS", "WRITE_MAX_CONCURRENCY", "SELF_ADDR", "MEMBERS", "PEERS", "PEER_SERVICE"} {
		t.Setenv(name, "")
	}
}

func TestFromEnvParsingDefaultsAndZeroSemantics(t *testing.T) {
	clearRuntimeEnv(t)
	c, err := FromEnv(8001, true)
	if err != nil || c.CacheBytes != 64<<20 || c.TTL != time.Minute {
		t.Fatal(c, err)
	}
	for _, tc := range []struct {
		name, value string
		valid       bool
	}{
		{"TTL_SECONDS", "0", false}, {"TTL_SECONDS", "0.5", false}, {"TTL_SECONDS", "9223372036854775807", false}, {"TTL_SECONDS", "3601", false}, {"TTL_SECONDS", "3600", true},
		{"CACHE_BYTES", "not-a-number", false}, {"CACHE_BYTES", "134217729", false},
		{"MAX_MISS_WAITERS", "0", false}, {"WRITE_MAX_CONCURRENCY", "0", false}, {"SOURCE_MAX_PENDING", "0", true},
		{"SOURCE_MAX_CONCURRENCY", "1.0", false}, {"SOURCE_MAX_CONCURRENCY", "1025", false}, {"API_ADDR", "127.0.0.1:65536", false},
	} {
		t.Run(tc.name+"/"+tc.value, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)
			actual, err := FromEnv(8001, true)
			if (err == nil) != tc.valid {
				t.Fatal(actual, err)
			}
			if tc.name == "SOURCE_MAX_PENDING" && err == nil && actual.SourceMaxPending != 0 {
				t.Fatal("explicit zero queue was defaulted")
			}
		})
	}
}

func TestSourceFactoryRequirementsDoNotApplyToInjectedSources(t *testing.T) {
	clearRuntimeEnv(t)
	c := runtimeTestConfig()
	c.SourceMode = "postgres"
	if err := c.Validate(); err != nil {
		t.Fatal("common validation required unused credentials", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, c); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatal("real startup accepted missing credentials", err)
	}
	source := cache.GetterFunc(func(context.Context, string) ([]byte, error) { return []byte("value"), nil })
	if err := RunWithSource(ctx, c, source); err != nil {
		t.Fatal("injected source required database credentials or rejected defaults/ephemeral ports", err)
	}
	c.SourceMode = "custom"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, c); err == nil || !strings.Contains(err.Error(), "SOURCE_MODE") {
		t.Fatal("real source factory accepted unknown mode", err)
	}
	if err := RunWithSource(ctx, c, source); err != nil {
		t.Fatal("injected source incorrectly required a built-in factory mode", err)
	}
	c.SourceMode = "postgres"
	if err := RunWithSource(ctx, c, nil); err == nil {
		t.Fatal("nil injected source accepted")
	}
	c.SourceConcurrency = 0
	c.DatabaseURL = "postgres://private-secret"
	c.AdminToken = "private-token"
	if err := Run(ctx, c); err == nil || !strings.Contains(err.Error(), "SOURCE_MAX_CONCURRENCY") || strings.Contains(err.Error(), "private-") {
		t.Fatal("source opened before configuration validation", err)
	}
}

func TestRemovedReadOptionsFailWithMigrationMessage(t *testing.T) {
	for _, old := range []string{"READ_ROUTING_MODE", "HOT_CACHE_BYTES", "PEERS", "PEER_SERVICE"} {
		t.Run(old, func(t *testing.T) {
			clearRuntimeEnv(t)
			t.Setenv(old, "peer")
			_, err := FromEnv(8001, true)
			if err == nil || !strings.Contains(err.Error(), "removed") {
				t.Fatal("obsolete option silently accepted", old, err)
			}
		})
	}
}
