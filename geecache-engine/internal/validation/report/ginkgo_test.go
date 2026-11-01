package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGinkgoCountsAndFailures(t *testing.T) {
	tests := []struct {
		name, report, status             string
		passed, failed, skipped, pending int
	}{
		{"passed", `[{"SuiteDescription":"Controller Suite","SuiteSucceeded":true,"SpecReports":[` + strings.Repeat(`{"LeafNodeType":"It","State":"passed"},`, 5) + `{"LeafNodeType":"It","State":"passed"},{"LeafNodeType":"BeforeSuite","State":"passed"},{"LeafNodeType":"AfterSuite","State":"passed"}]}]`, "passed", 6, 0, 0, 0},
		{"skip-pending", `[{"SuiteSucceeded":true,"SpecReports":[{"LeafNodeType":"It","State":"passed"},{"LeafNodeType":"It","State":"skipped"},{"LeafNodeType":"It","State":"pending"}]}]`, "passed", 1, 0, 1, 1},
		{"failure-states", `[{"SuiteSucceeded":false,"SpecReports":[{"LeafNodeType":"It","State":"failed"},{"LeafNodeType":"It","State":"timedout"},{"LeafNodeType":"It","State":"panicked"},{"LeafNodeType":"It","State":"interrupted"},{"LeafNodeType":"It","State":"aborted"}]}]`, "failed", 0, 5, 0, 0},
		{"hook-failure", `[{"SuiteSucceeded":false,"SpecReports":[{"LeafNodeType":"BeforeSuite","State":"failed","Failure":{"Message":"API server unavailable"}}]}]`, "failed", 0, 0, 0, 0},
		{"suite-failure", `[{"SuiteSucceeded":false,"SpecReports":[{"LeafNodeType":"It","State":"passed"}]}]`, "failed", 1, 0, 0, 0},
		{"inconsistent-suite", `[{"SuiteSucceeded":true,"SpecReports":[{"LeafNodeType":"It","State":"failed"}]}]`, "failed", 0, 1, 0, 0},
		{"multiple-suites", `[{"SuiteSucceeded":true,"SpecReports":[{"LeafNodeType":"It","State":"passed"}]},{"SuiteSucceeded":true,"SpecReports":[{"LeafNodeType":"It","State":"passed"}]}]`, "passed", 2, 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeReportFixture(t, dir, "stages.tsv", "operator-tests\t0\t1\n")
			writeReportFixture(t, dir, "ginkgo-required", "")
			writeReportFixture(t, dir, "ginkgo.json", tt.report)
			// Go suite wrappers must not be counted in addition to Ginkgo specs.
			writeReportFixture(t, dir, "operator-tests.jsonl", "{\"Action\":\"pass\",\"Test\":\"TestControllers\"}\n")
			r, err := Generate(dir)
			if err != nil {
				t.Fatal(err)
			}
			s := r.Stages[0]
			if r.Status != tt.status || s.CountSource != "ginkgo-specs" {
				t.Fatalf("%+v", r)
			}
			if s.Passed == nil || s.Failed == nil || s.Skipped == nil || s.Pending == nil {
				t.Fatal("missing collected counts")
			}
			if *s.Passed != tt.passed || *s.Failed != tt.failed || *s.Skipped != tt.skipped || *s.Pending != tt.pending {
				t.Fatalf("unexpected counts: %+v", s)
			}
		})
	}
}

func TestUncollectedGinkgoCountsAreNull(t *testing.T) {
	tests := []struct {
		name, report string
		required     bool
		status       string
	}{
		{"legacy-missing", "", false, "passed"},
		{"required-missing", "", true, "failed"},
		{"corrupt", "{", true, "failed"},
		{"empty", "[]", true, "failed"},
		{"missing-success", `[{"SpecReports":[]}]`, true, "failed"},
		{"missing-specs", `[{"SuiteSucceeded":true}]`, true, "failed"},
		{"missing-node-type", `[{"SuiteSucceeded":true,"SpecReports":[{"State":"passed"}]}]`, true, "failed"},
		{"unknown-state", `[{"SuiteSucceeded":true,"SpecReports":[{"LeafNodeType":"It","State":"new-state"}]}]`, true, "failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeReportFixture(t, dir, "stages.tsv", "operator-tests\t0\t1\n")
			if tt.required {
				writeReportFixture(t, dir, "ginkgo-required", "")
			}
			if tt.report != "" {
				writeReportFixture(t, dir, "ginkgo.json", tt.report)
			}
			r, err := Generate(dir)
			if err != nil {
				t.Fatal(err)
			}
			s := r.Stages[0]
			if r.Status != tt.status || s.Passed != nil || s.Failed != nil || s.Skipped != nil || s.Pending != nil {
				t.Fatalf("%+v", r)
			}
			b, err := os.ReadFile(filepath.Join(dir, "report.json"))
			if err != nil {
				t.Fatal(err)
			}
			var document struct{ Stages []map[string]any }
			if err = json.Unmarshal(b, &document); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"tests_passed", "tests_failed", "tests_skipped", "tests_pending"} {
				v, exists := document.Stages[0][key]
				if !exists || v != nil {
					t.Fatalf("%s should be null", key)
				}
			}
			md, err := os.ReadFile(filepath.Join(dir, "report.md"))
			if err != nil || !strings.Contains(string(md), "| N/A | N/A | N/A | N/A |") {
				t.Fatalf("N/A missing: %v", err)
			}
		})
	}
}

func TestGinkgoCommandFailureOverridesSuccessfulSpecs(t *testing.T) {
	dir := t.TempDir()
	writeReportFixture(t, dir, "stages.tsv", "operator-tests\t1\t1\n")
	writeReportFixture(t, dir, "ginkgo.json", `[{"SuiteSucceeded":true,"SpecReports":[{"LeafNodeType":"It","State":"passed"}]}]`)
	r, err := Generate(dir)
	if err != nil || r.Status != "failed" {
		t.Fatalf("report=%+v err=%v", r, err)
	}
}

func writeReportFixture(t *testing.T, dir, name, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
