package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
)

type Payload struct {
	Key     string `json:"key"`
	Version int64  `json:"version"`
	Padding string `json:"padding"`
}

func Key(prefix string, index int) string { return fmt.Sprintf("%s-%04d", prefix, index) }

func Value(key string, version int64, size int) []byte {
	p := Payload{Key: key, Version: version}
	base, _ := json.Marshal(p)
	padding := strings.Builder{}
	for i := 0; padding.Len() < size-len(base); i++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d", key, version, i)))
		padding.WriteString(hex.EncodeToString(sum[:]))
	}
	if size > len(base) {
		p.Padding = padding.String()[:size-len(base)]
	}
	b, _ := json.Marshal(p)
	return b
}

func readTrace(c Config) []int {
	trace := make([]int, c.Reads)
	rng := rand.New(rand.NewSource(c.Seed))
	z := rand.NewZipf(rng, 1.2, 1, uint64(c.Keys-1))
	for i := range trace {
		switch c.Scenario {
		case "warm-hot", "cold-key":
			trace[i] = 0
		case "zipf", "mixed":
			trace[i] = int(z.Uint64())
		default:
			trace[i] = i % c.Keys
		}
	}
	return trace
}
