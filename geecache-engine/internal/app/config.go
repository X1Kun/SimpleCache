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
	"github.com/X1Kun/simpleCache/geecache-engine/internal/cluster"
)

type Config struct {
	ClusterAddr, APIAddr, SelfID, DiscoveryMode string
	Namespace, ClusterService                   string
	API                                         bool
	CacheBytes                                  int64
	TTL                                         time.Duration
	SourceConcurrency                           int
	SourceMaxPending                            int
	MaxMissWaiters                              int
	WriteConcurrency                            int
	Members                                     []cluster.Member
	DebugAddr                                   string
	ProfileContention                           bool
	SourceMode, DatabaseURL, AdminToken         string
}

// FromEnv binds environment strings to a runtime configuration. Explicit zero
// values for TTL/waiters/write concurrency remain invalid environment settings;
// programmatic zero values retain their existing default semantics.
func FromEnv(port int, api bool) (Config, error) {
	for _, old := range []string{"READ_ROUTING_MODE", "HOT_CACHE_BYTES", "PEERS", "PEER_SERVICE"} {
		if os.Getenv(old) != "" {
			return Config{}, fmt.Errorf("%s was removed; use Gateway routing and MEMBERS/CLUSTER_SERVICE for invalidation discovery", old)
		}
	}

	c := Config{ClusterAddr: fmt.Sprintf("0.0.0.0:%d", port), APIAddr: "0.0.0.0:9999", API: api,
		CacheBytes: 64 << 20, TTL: time.Minute, SourceConcurrency: 32, SourceMaxPending: cache.DefaultSourcePending,
		MaxMissWaiters: cache.DefaultMissWaiters, WriteConcurrency: DefaultWriteConcurrency,
		DiscoveryMode: os.Getenv("DISCOVERY_MODE"), Namespace: os.Getenv("POD_NAMESPACE"), ClusterService: os.Getenv("CLUSTER_SERVICE"),
		SourceMode:  os.Getenv("SOURCE_MODE"),
		DatabaseURL: os.Getenv("DATABASE_URL"), AdminToken: os.Getenv("CACHE_ADMIN_TOKEN"), DebugAddr: os.Getenv("DEBUG_ADDR")}
	if port < 1 || port > 65535 {
		return c, fmt.Errorf("invalid notification port")
	}
	if c.DiscoveryMode == "" {
		c.DiscoveryMode = "static"
	}
	if c.SourceMode == "" {
		c.SourceMode = "demo"
	}
	if raw := os.Getenv("API_ADDR"); raw != "" {
		c.APIAddr = raw
	}
	for _, item := range []struct {
		name  string
		value *int64
	}{{"CACHE_BYTES", &c.CacheBytes}} {
		if raw := os.Getenv(item.name); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return c, fmt.Errorf("invalid %s", item.name)
			}
			*item.value = n
		}
	}
	if raw := os.Getenv("TTL_SECONDS"); raw != "" {
		if _, err := strconv.ParseInt(raw, 10, 64); err != nil {
			return c, fmt.Errorf("invalid TTL_SECONDS")
		}
		// ParseDuration rejects conversion overflow before range validation.
		duration, err := time.ParseDuration(raw + "s")
		if err != nil || duration == 0 {
			return c, fmt.Errorf("invalid TTL_SECONDS")
		}
		c.TTL = duration
	}
	for _, item := range []struct {
		name        string
		value       *int
		zeroDefault bool
	}{
		{"SOURCE_MAX_CONCURRENCY", &c.SourceConcurrency, false}, {"SOURCE_MAX_PENDING", &c.SourceMaxPending, false},
		{"MAX_MISS_WAITERS", &c.MaxMissWaiters, true}, {"WRITE_MAX_CONCURRENCY", &c.WriteConcurrency, true}} {
		if raw := os.Getenv(item.name); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || (n == 0 && item.zeroDefault) {
				return c, fmt.Errorf("invalid %s", item.name)
			}
			*item.value = n
		}
	}
	switch c.DiscoveryMode {
	case "kubernetes":
		pod := os.Getenv("POD_NAME")
		if pod == "" {
			return c, fmt.Errorf("Kubernetes discovery requires POD_NAME, POD_NAMESPACE and CLUSTER_SERVICE")
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
		members := os.Getenv("MEMBERS")
		if members == "" {
			members = self
		}
		for _, raw := range strings.Split(members, ",") {
			address, err := staticAddress(strings.TrimSpace(raw))
			if err != nil {
				return c, err
			}
			c.Members = append(c.Members, cluster.Member{ID: address, Address: address})
		}
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, c.validateSourceConfig()
}

// Validate checks common runtime constraints without reading environment,
// mutating defaults, performing I/O, or requiring credentials for an injected
// source. Zero TTL/miss/write budgets retain their component defaults; zero
// SourceMaxPending means no queue. Listener port zero requests an ephemeral port.
func (c Config) Validate() error {
	if err := validateListenAddr(c.ClusterAddr); err != nil {
		return fmt.Errorf("invalid ClusterAddr")
	}
	if c.API || c.APIAddr != "" {
		if err := validateListenAddr(c.APIAddr); err != nil {
			return fmt.Errorf("invalid API_ADDR")
		}
	}
	if c.SelfID == "" {
		return fmt.Errorf("SelfID is required")
	}
	if err := validateDebugAddr(c.DebugAddr); err != nil {
		return err
	}
	if c.ProfileContention && c.DebugAddr == "" {
		return fmt.Errorf("contention profiling requires DEBUG_ADDR")
	}
	if c.CacheBytes < 1<<20 || c.CacheBytes > 128<<20 {
		return fmt.Errorf("invalid CACHE_BYTES")
	}
	if c.TTL < 0 || c.TTL > time.Hour {
		return fmt.Errorf("invalid TTL_SECONDS")
	}
	if c.SourceConcurrency < 1 || c.SourceConcurrency > 1024 {
		return fmt.Errorf("invalid SOURCE_MAX_CONCURRENCY")
	}
	if c.SourceMaxPending < 0 || c.SourceMaxPending > 4096 {
		return fmt.Errorf("invalid SOURCE_MAX_PENDING")
	}
	if c.MaxMissWaiters < 0 || c.MaxMissWaiters > 4096 {
		return fmt.Errorf("invalid MAX_MISS_WAITERS")
	}
	if c.WriteConcurrency < 0 || c.WriteConcurrency > 256 {
		return fmt.Errorf("invalid WRITE_MAX_CONCURRENCY")
	}
	switch c.DiscoveryMode {
	case "static":
	case "kubernetes":
		namespace, pod, ok := strings.Cut(c.SelfID, "/")
		if c.Namespace == "" || c.ClusterService == "" || !ok || namespace != c.Namespace || pod == "" || strings.Contains(pod, "/") {
			return fmt.Errorf("Kubernetes discovery requires namespace/pod SelfID, POD_NAMESPACE and CLUSTER_SERVICE")
		}
	default:
		return fmt.Errorf("unsupported DISCOVERY_MODE")
	}
	for _, member := range c.Members {
		if member.ID == "" || strings.ContainsAny(member.Address, "/?#") {
			return fmt.Errorf("invalid member")
		}
		if _, err := staticAddress("http://" + member.Address); err != nil {
			return fmt.Errorf("invalid member address")
		}
	}
	return nil
}

// Only Run/FromEnv select and open the configured source. RunWithSource uses
// the supplied Getter and therefore does not require DATABASE_URL/admin token.
func (c Config) validateSourceConfig() error {
	if c.SourceMode != "" && c.SourceMode != "demo" && c.SourceMode != "postgres" {
		return fmt.Errorf("unsupported SOURCE_MODE")
	}
	if c.SourceMode == "postgres" && (c.DatabaseURL == "" || c.AdminToken == "") {
		return fmt.Errorf("postgres mode requires DATABASE_URL and CACHE_ADMIN_TOKEN")
	}
	return nil
}

func validateListenAddr(address string) error {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("invalid listener port")
	}
	return nil
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
		return "", fmt.Errorf("invalid static member address")
	}
	host, port, err := net.SplitHostPort(u.Host)
	number, e := strconv.Atoi(port)
	if err != nil || e != nil || host == "" || number < 1 || number > 65535 {
		return "", fmt.Errorf("invalid static member address")
	}
	return net.JoinHostPort(strings.ToLower(host), strconv.Itoa(number)), nil
}
