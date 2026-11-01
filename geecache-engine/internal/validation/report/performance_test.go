package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/bench/diagnostics"
)

func TestPerformanceReportSeparatesProfiledTrials(t *testing.T) {
	for _, failure := range []string{"", "same-process", "wrong-value", "incomplete", "duplicate", "missing"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			writeReportFixture(t, dir, "stages.tsv", "performance\t0\t1\n")
			summary := diagnostics.Summary{Status: "passed", Seconds: 10, Repeats: 3, Workers: 8, ThinkMS: 1, SourceDelayMS: 100, ServerGOMAXPROCS: 2, ServerSHA256: strings.Repeat("a", 64)}
			for _, profiled := range []bool{false, true} {
				repeats := 3
				if profiled {
					repeats = 1
				}
				for repeat := 1; repeat <= repeats; repeat++ {
					for _, scenario := range []string{"warm-hot", "distinct-keys", "capacity-pressure"} {
						phase := "baseline"
						p95 := float64(repeat)
						if profiled {
							phase = "profile"
							p95 = 999
						}
						summary.Trials = append(summary.Trials, diagnostics.Trial{ID: phase + "-" + scenario + "-" + string(rune('0'+repeat)), Scenario: scenario, Repeat: repeat, Profiled: profiled, ServerPID: 2, ClientPID: 1, Requests: 10, P95: p95, P99: p95, CPUSeconds: 1})
					}
				}
			}
			switch failure {
			case "same-process":
				summary.Trials[0].ServerPID = 1
			case "wrong-value":
				summary.Trials[0].Incorrect = 1
			case "incomplete":
				summary.Trials = summary.Trials[:1]
			case "duplicate":
				summary.Trials[1] = summary.Trials[0]
			}
			if failure != "missing" {
				b, err := json.Marshal(summary)
				if err != nil {
					t.Fatal(err)
				}
				writeReportFixture(t, dir, "performance.json", string(b))
			}
			r, err := Generate(dir)
			if err != nil {
				t.Fatal(err)
			}
			want := "passed"
			if failure != "" {
				want = "failed"
			}
			if r.Status != want {
				t.Fatalf("%+v", r)
			}
			if failure == "" {
				if r.Stages[0].Passed == nil || *r.Stages[0].Passed != 12 {
					t.Fatal("trial count missing")
				}
				md, err := os.ReadFile(filepath.Join(dir, "report.md"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(md), "| warm-hot | 3 | 2.000 [1.000, 3.000]") {
					t.Fatal("profile overhead contaminated baseline aggregate")
				}
			}
		})
	}
}
