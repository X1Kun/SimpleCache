package gateway

import (
	"math"
	"sort"
)

type Record struct {
	Dispatched  bool    `json:"dispatched,omitempty"`
	DispatchMS  float64 `json:"dispatch_lag_ms,omitempty"`
	Stage       string  `json:"stage,omitempty"`
	Cohort      string  `json:"cohort,omitempty"`
	ScheduledMS float64 `json:"scheduled_ms,omitempty"`
	QueueMS     float64 `json:"queue_ms,omitempty"`
	TotalMS     float64 `json:"total_ms,omitempty"`
	DropReason  string  `json:"drop_reason,omitempty"`

	Index            int     `json:"index"`
	Method           string  `json:"method"`
	KeyIndex         int     `json:"key_index"`
	Attempted        bool    `json:"attempted"`
	Status           int     `json:"status"`
	ElapsedMS        float64 `json:"elapsed_ms"`
	Node             string  `json:"node,omitempty"`
	Version          int64   `json:"version,omitempty"`
	CommittedAtStart int64   `json:"committed_at_start,omitempty"`
	Error            string  `json:"error,omitempty"`
	Incorrect        bool    `json:"incorrect"`
	StaleAtStart     bool    `json:"stale_at_start"`
}

type Percentiles struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50_ms"`
	P95   float64 `json:"p95_ms"`
	P99   float64 `json:"p99_ms"`
}

func Quantiles(values []float64) Percentiles {
	if len(values) == 0 {
		return Percentiles{}
	}
	v := append([]float64(nil), values...)
	sort.Float64s(v)
	at := func(p float64) float64 { return v[int(math.Ceil(p*float64(len(v))))-1] }
	return Percentiles{len(v), at(.5), at(.95), at(.99)}
}

type Summary struct {
	Arrival          *ArrivalReport `json:"arrival,omitempty"`
	Config           Config         `json:"config"`
	GoVersion        string         `json:"go_version"`
	GOMAXPROCS       int            `json:"gomaxprocs"`
	TraceSHA256      string         `json:"trace_sha256"`
	Planned          int            `json:"planned"`
	Attempted        int            `json:"attempted"`
	Skipped          int            `json:"skipped"`
	Success          int            `json:"success"`
	Errors           int            `json:"errors"`
	Incorrect        int            `json:"incorrect"`
	Rejections       int            `json:"http_503"`
	StaleAtStart     int            `json:"stale_at_start"`
	Writes           int            `json:"writes"`
	ElapsedSeconds   float64        `json:"elapsed_seconds"`
	SuccessPerSecond float64        `json:"success_per_second"`
	Latency          Percentiles    `json:"latency_all_attempts"`
	ReadLatency      Percentiles    `json:"read_latency"`
	WriteLatency     Percentiles    `json:"write_latency"`
	StatusCounts     map[int]int    `json:"statuses"`
}
