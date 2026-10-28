package app

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/peer"
)

type Config struct {
	PeerAddr, APIAddr, SelfID, DiscoveryMode string
	Namespace, PeerService                   string
	API                                      bool
	CacheBytes                               int64
	HotCacheBytes                            int64
	TTL                                      time.Duration
	SourceConcurrency                        int
	SourceMaxPending                         int
	MaxMissWaiters                           int
	WriteConcurrency                         int
	Members                                  []peer.Member
	DebugAddr                                string
	ProfileContention                        bool
	SourceMode, DatabaseURL, AdminToken      string
	ReadRoutingMode                          string
}

func FromEnv(port int, api bool) (Config, error) {
	c := Config{PeerAddr: fmt.Sprintf("0.0.0.0:%d", port), APIAddr: "0.0.0.0:9999", API: api,
		CacheBytes: 64 << 20, TTL: time.Minute, SourceConcurrency: 32, DiscoveryMode: os.Getenv("DISCOVERY_MODE"),
		Namespace: os.Getenv("POD_NAMESPACE"), PeerService: os.Getenv("PEER_SERVICE"),
		SourceMaxPending: cache.DefaultSourcePending, MaxMissWaiters: cache.DefaultMissWaiters, WriteConcurrency: DefaultWriteConcurrency,
	}
	if port < 1 || port > 65535 {
		return c, fmt.Errorf("invalid peer port")
	}
	if c.DiscoveryMode == "" {
		c.DiscoveryMode = "static"
	}
	c.ReadRoutingMode = os.Getenv("READ_ROUTING_MODE")
	if c.ReadRoutingMode == "" {
		c.ReadRoutingMode = "peer"
	}
	if c.ReadRoutingMode != "peer" && c.ReadRoutingMode != "gateway" {
		return c, fmt.Errorf("unsupported READ_ROUTING_MODE")
	}
	c.SourceMode = os.Getenv("SOURCE_MODE")
	if c.SourceMode == "" {
		c.SourceMode = "demo"
	}
	c.DatabaseURL, c.AdminToken = os.Getenv("DATABASE_URL"), os.Getenv("CACHE_ADMIN_TOKEN")
	if c.SourceMode != "demo" && c.SourceMode != "postgres" {
		return c, fmt.Errorf("unsupported SOURCE_MODE")
	}
	if c.SourceMode == "postgres" && (c.DatabaseURL == "" || c.AdminToken == "") {
		return c, fmt.Errorf("postgres mode requires DATABASE_URL and CACHE_ADMIN_TOKEN")
	}
	if v := os.Getenv("API_ADDR"); v != "" {
		c.APIAddr = v
	}
	if _, _, err := net.SplitHostPort(c.APIAddr); err != nil {
		return c, fmt.Errorf("invalid API_ADDR: %w", err)
	}
	c.DebugAddr = os.Getenv("DEBUG_ADDR")
	if err := validateDebugAddr(c.DebugAddr); err != nil {
		return c, err
	}
	for _, item := range []struct {
		name  string
		value *int64
	}{{"CACHE_BYTES", &c.CacheBytes}} {
		if v := os.Getenv(item.name); v != "" {
			parsed, err := strconv.ParseInt(v, 10, 64)
			if err != nil || parsed < 1<<20 || parsed > 128<<20 {
				return c, fmt.Errorf("invalid %s", item.name)
			}
			*item.value = parsed
		}
	}
	if v := os.Getenv("TTL_SECONDS"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 1 || parsed > 3600 {
			return c, fmt.Errorf("invalid TTL_SECONDS")
		}
		c.TTL = time.Duration(parsed) * time.Second
	}
	if v := os.Getenv("HOT_CACHE_BYTES"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil || parsed < 0 || parsed >= c.CacheBytes {
			return c, fmt.Errorf("invalid HOT_CACHE_BYTES")
		}
		c.HotCacheBytes = parsed
	}
	if c.ReadRoutingMode == "gateway" && c.HotCacheBytes != 0 {
		return c, fmt.Errorf("gateway routing requires HOT_CACHE_BYTES=0")
	}
	if v := os.Getenv("SOURCE_MAX_CONCURRENCY"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 1 || parsed > 1024 {
			return c, fmt.Errorf("invalid SOURCE_MAX_CONCURRENCY")
		}
		c.SourceConcurrency = parsed
	}
	for _, item := range []struct {
		name             string
		value            *int
		minimum, maximum int
	}{
		{"SOURCE_MAX_PENDING", &c.SourceMaxPending, 0, 4096},
		{"MAX_MISS_WAITERS", &c.MaxMissWaiters, 1, 4096},
		{"WRITE_MAX_CONCURRENCY", &c.WriteConcurrency, 1, 256},
	} {
		if raw := os.Getenv(item.name); raw != "" {
			v, err := strconv.Atoi(raw)
			if err != nil || v < item.minimum || v > item.maximum {
				return c, fmt.Errorf("invalid %s", item.name)
			}
			*item.value = v
		}
	}
	switch c.DiscoveryMode {
	case "kubernetes":
		pod := os.Getenv("POD_NAME")
		if pod == "" || c.Namespace == "" || c.PeerService == "" {
			return c, fmt.Errorf("Kubernetes discovery requires POD_NAME, POD_NAMESPACE and PEER_SERVICE")
		}
		c.SelfID = c.Namespace + "/" + pod
	case "static":
		self := os.Getenv("SELF_ADDR")
		if self == "" {
			self = fmt.Sprintf("http://localhost:%d", port)
		}
		address, err := staticAddress(self)
		if err != nil {
			return c, err
		}
		c.SelfID = address
		peers := os.Getenv("PEERS")
		if peers == "" {
			peers = self
		}
		for _, item := range strings.Split(peers, ",") {
			address, err := staticAddress(strings.TrimSpace(item))
			if err != nil {
				return c, err
			}
			c.Members = append(c.Members, peer.Member{ID: address, Address: address})
		}
	default:
		return c, fmt.Errorf("unsupported DISCOVERY_MODE %q", c.DiscoveryMode)
	}
	return c, nil
}

// Debug listeners accept literal loopback addresses only, never DNS or wildcards.
func validateDebugAddr(address string) error {
	if address == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(address)
	number, parseErr := strconv.Atoi(port)
	ip := net.ParseIP(host)
	if err != nil || parseErr != nil || number < 0 || number > 65535 || ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("DEBUG_ADDR requires a literal loopback IP and port")
	}
	return nil
}
func staticAddress(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("invalid static peer %q", raw)
	}
	host, port, err := net.SplitHostPort(u.Host)
	number, e := strconv.Atoi(port)
	if err != nil || e != nil || host == "" || number < 1 || number > 65535 {
		return "", fmt.Errorf("invalid static peer %q", raw)
	}
	return net.JoinHostPort(strings.ToLower(host), strconv.Itoa(number)), nil
}
