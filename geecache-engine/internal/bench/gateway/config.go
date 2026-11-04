// Package gateway implements deterministic in-cluster workload measurements.
package gateway

import (
	"fmt"
	"net/url"
)

type RateStage struct {
	Name    string `json:"name"`
	Rate    int    `json:"rate"`
	Seconds int    `json:"seconds"`
}

type Config struct {
	MaxScheduleLagMillis int         `json:"max_schedule_lag_ms,omitempty"`
	MaxCatchUp           int         `json:"max_catch_up,omitempty"`
	DatasetProfile       string      `json:"dataset_profile,omitempty"`
	KeyStyle             string      `json:"key_style,omitempty"`
	Distribution         string      `json:"distribution,omitempty"`
	MaxPending           int         `json:"max_pending,omitempty"`
	Stages               []RateStage `json:"stages,omitempty"`
	Endpoint             string      `json:"endpoint"`
	Host                 string      `json:"host"`
	Prefix               string      `json:"prefix"`
	Scenario             string      `json:"scenario"`
	Keys                 int         `json:"keys"`
	ValueBytes           int         `json:"value_bytes"`
	Reads                int         `json:"reads"`
	Workers              int         `json:"workers"`
	ThinkMillis          int         `json:"think_ms"`
	Seed                 int64       `json:"seed"`
	Windows              int         `json:"windows"`
	DurationSeconds      int         `json:"duration_seconds"`
}

func Validate(c Config) error {
	u, e := url.Parse(c.Endpoint)
	if e != nil || u.Scheme != "http" || u.Host == "" || u.RawQuery != "" || u.User != nil || u.Path != "" || u.Fragment != "" {
		return fmt.Errorf("invalid endpoint")
	}
	if c.Host == "" || c.Prefix == "" || len(c.Prefix) > 80 || c.Keys < 1 || c.Keys > 50000 || c.Workers < 1 || c.Workers > 128 || c.ValueBytes < 128 || c.ValueBytes > 1<<20 || c.Reads < 1 || c.Reads > 1_000_000 || c.ThinkMillis < 0 || c.ThinkMillis > 100 || c.Windows < 0 || c.Windows > 10000 || c.DurationSeconds < 0 || c.DurationSeconds > 120 {
		return fmt.Errorf("invalid bounded workload options")
	}
	switch c.Scenario {
	case "warm-hot", "capacity", "zipf", "cold-key", "mixed", "ttl", "arrival":
	default:
		return fmt.Errorf("invalid scenario")
	}
	if c.Scenario == "mixed" && c.Windows == 0 {
		return fmt.Errorf("mixed workload requires windows")
	}
	if c.Scenario == "ttl" && c.DurationSeconds < 5 {
		return fmt.Errorf("TTL workload requires a duration")
	}
	if c.Scenario == "arrival" {
		if err := validateArrival(c); err != nil {
			return err
		}
	} else if c.Distribution != "" || c.MaxPending != 0 || c.MaxScheduleLagMillis != 0 || c.MaxCatchUp != 0 {
		return fmt.Errorf("arrival options require arrival scenario")
	}
	if err := validateDataset(c); err != nil {
		return err
	}

	if c.Scenario != "arrival" && len(c.Stages) != 0 {
		return fmt.Errorf("stages require arrival scenario")
	}
	return nil
}
