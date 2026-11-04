// Package measurement contains deterministic summaries shared by tool clients.
package measurement

import (
	"math"
	"sort"
)

type Percentiles struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50_ms"`
	P95   float64 `json:"p95_ms"`
	P99   float64 `json:"p99_ms"`
}

// Quantiles uses nearest rank, preserves the input, and reports zero count for
// no observations. It does not assign latency to unattempted/dropped requests.
func Quantiles(values []float64) Percentiles {
	if len(values) == 0 {
		return Percentiles{}
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	at := func(p float64) float64 { return sorted[int(math.Ceil(p*float64(len(sorted))))-1] }
	return Percentiles{Count: len(sorted), P50: at(.5), P95: at(.95), P99: at(.99)}
}
