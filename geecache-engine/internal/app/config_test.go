package app

import (
	"testing"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
)

func TestConfigRejectsIncompleteKubernetesDiscovery(t *testing.T) {
	t.Setenv("DISCOVERY_MODE", "kubernetes")
	t.Setenv("POD_NAME", "")
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("PEER_SERVICE", "")
	if _, err := FromEnv(8001, true); err == nil {
		t.Fatal("incomplete Kubernetes identity accepted")
	}
	t.Setenv("DISCOVERY_MODE", "static")
	t.Setenv("SELF_ADDR", "")
	t.Setenv("PEERS", "")
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
	t.Setenv("PEER_SERVICE", "cache-svc")
	t.Setenv("SELF_ADDR", "http://stale-address:8001")
	t.Setenv("PEERS", "http://stale-address:8001")
	c, err := FromEnv(8001, true)
	if err != nil || c.SelfID != "demo/cache-1" || len(c.Members) != 0 {
		t.Fatalf("unexpected Kubernetes configuration: %+v %v", c, err)
	}
	t.Setenv("DISCOVERY_MODE", "unknown")
	if _, err := FromEnv(8001, true); err == nil {
		t.Fatal("unknown discovery mode accepted")
	}
}

func TestHotCacheConfigurationSharesCapacity(t *testing.T) {
	t.Setenv("CACHE_BYTES", "1048576")
	for _, value := range []string{"-1", "1048576", "not-a-number"} {
		t.Setenv("HOT_CACHE_BYTES", value)
		if _, err := FromEnv(8001, true); err == nil {
			t.Fatal("invalid hot budget accepted")
		}
	}
	t.Setenv("HOT_CACHE_BYTES", "131072")
	c, err := FromEnv(8001, true)
	if err != nil || c.HotCacheBytes != 131072 {
		t.Fatal(c.HotCacheBytes, err)
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

func TestReadRoutingConfiguration(t *testing.T) {
	t.Setenv("READ_ROUTING_MODE", "gateway")
	t.Setenv("HOT_CACHE_BYTES", "0")
	c, err := FromEnv(8001, true)
	if err != nil || c.ReadRoutingMode != "gateway" {
		t.Fatal(c, err)
	}
	t.Setenv("HOT_CACHE_BYTES", "1024")
	if _, err := FromEnv(8001, true); err == nil {
		t.Fatal("gateway accepted hot cache")
	}
	t.Setenv("READ_ROUTING_MODE", "other")
	if _, err := FromEnv(8001, true); err == nil {
		t.Fatal("invalid routing accepted")
	}
}
