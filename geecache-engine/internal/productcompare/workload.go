package productcompare

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/peer/hashring"
)

var nodeIDs = []string{"node-0", "node-1", "node-2"}
var scenarios = []string{"cold-key", "capacity", "updates", "node-failure", "zipf-read", "mixed-updates"}

type Operation struct {
	Node    int    `json:"node"`
	Key     string `json:"key"`
	Version int    `json:"version"`
}

type mixedWindow struct {
	Write Operation   `json:"write"`
	Reads []Operation `json:"reads"`
}

func zipfTrace(keys []string, count int, seed int64) []Operation {
	random := rand.New(rand.NewSource(seed))
	zipf := rand.NewZipf(random, 1.2, 1, uint64(len(keys)-1))
	trace := make([]Operation, count)
	for i := range trace {
		trace[i] = Operation{Node: random.Intn(3), Key: keys[zipf.Uint64()], Version: 1}
	}
	return trace
}

func mixedTrace(keys []string, windows int, seed int64) []mixedWindow {
	random := rand.New(rand.NewSource(seed))
	zipf := rand.NewZipf(random, 1.2, 1, uint64(len(keys)-1))
	trace := make([]mixedWindow, windows)
	for i := range trace {
		trace[i].Write = Operation{Node: i % 3, Key: keys[0], Version: i + 2}
		for j := 0; j < 19; j++ {
			key := keys[zipf.Uint64()]
			version := 1
			if key == keys[0] {
				version = 0
			} // checked against the submitted version interval
			trace[i].Reads = append(trace[i].Reads, Operation{Node: random.Intn(3), Key: key, Version: version})
		}
	}
	return trace
}

func modesFor(scenario string) []string {
	if scenario == "mixed-updates" {
		return []string{"independent", "coordinated", "coordinated-global", "coordinated-hot"}
	}
	if scenario == "zipf-read" {
		return []string{"independent", "coordinated", "coordinated-hot"}
	}
	return []string{"independent", "coordinated"}
}

func keysFor(seed int64, count int) []string {
	keys := make([]string, count)
	for i := range keys {
		keys[i] = fmt.Sprintf("product:%x", sha256.Sum256([]byte(fmt.Sprintf("%d:%d", seed, i))))
	}
	return keys
}

func productValue(key string, version, size int) []byte {
	value := struct {
		Key     string `json:"key"`
		Version int    `json:"version"`
		Padding string `json:"padding"`
	}{Key: key, Version: version}
	base, _ := json.Marshal(value)
	var padding strings.Builder
	for i := 0; padding.Len() < size-len(base); i++ {
		fmt.Fprintf(&padding, "%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d", key, version, i))))
	}
	if size > len(base) {
		value.Padding = padding.String()[:size-len(base)]
	}
	result, _ := json.Marshal(value)
	return result
}

func owners(keys []string, valueBytes int) (map[string]int64, int64) {
	ring := hashring.New(100, nil)
	ring.Add(nodeIDs...)
	bytes := map[string]int64{}
	var total int64
	for _, key := range keys {
		n := int64(len(key) + valueBytes)
		bytes[ring.Get(key)] += n
		total += n
	}
	return bytes, total
}

func capacityTrace(keys []string, passes int) []Operation {
	trace := make([]Operation, 0, len(keys)*3*passes)
	for pass := 0; pass < passes; pass++ {
		for _, key := range keys {
			for node := 0; node < 3; node++ {
				trace = append(trace, Operation{Node: node, Key: key, Version: 1})
			}
		}
	}
	return trace
}

func burstTrace(key string, count int, nodes []int) []Operation {
	trace := make([]Operation, count)
	for i := range trace {
		trace[i] = Operation{Node: nodes[i%len(nodes)], Key: key, Version: 1}
	}
	return trace
}

func failureKey(keys []string) string {
	ring := hashring.New(100, nil)
	ring.Add(nodeIDs...)
	for _, key := range keys {
		if ring.Get(key) == "node-2" {
			return key
		}
	}
	return ""
}

func traceHash(trace []Operation) string {
	return valueHash(trace)
}

func valueHash(value any) string {
	b, _ := json.Marshal(value)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
