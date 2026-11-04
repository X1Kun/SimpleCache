// Package validationreport preserves success and failure evidence for each run.
package report

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/bench/diagnostics"
)

type Stage struct {
	Name        string            `json:"name"`
	ExitCode    int               `json:"exit_code"`
	Seconds     int               `json:"seconds"`
	Passed      *int              `json:"tests_passed"`
	Failed      *int              `json:"tests_failed"`
	Skipped     *int              `json:"tests_skipped"`
	Pending     *int              `json:"tests_pending"`
	CountSource string            `json:"count_source,omitempty"`
	Notes       []string          `json:"notes,omitempty"`
	Failures    []string          `json:"failures,omitempty"`
	Evidence    []json.RawMessage `json:"evidence,omitempty"`
}
type Report struct {
	Status       string               `json:"status"`
	GeneratedAt  string               `json:"generated_at"`
	RunDirectory string               `json:"run_directory"`
	GoVersion    string               `json:"go_version"`
	Platform     string               `json:"platform"`
	CPUs         int                  `json:"logical_cpus"`
	Stages       []Stage              `json:"stages"`
	Artifacts    []string             `json:"artifacts"`
	Performance  *diagnostics.Summary `json:"performance,omitempty"`
}

func parseEvents(path string, s *Stage) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	passed, failed, skipped, pending := 0, 0, 0, 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	for scanner.Scan() {
		var e struct{ Action, Package, Test, Output string }
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			return fmt.Errorf("invalid test event in %s: %w", path, err)
		}
		if e.Test != "" {
			switch e.Action {
			case "pass":
				passed++
			case "fail":
				failed++
				s.Failures = append(s.Failures, e.Package+"/"+e.Test)
			case "skip":
				skipped++
			}
		}
		if e.Action == "fail" && e.Test == "" {
			s.Failures = append(s.Failures, "package "+e.Package)
		}
		if _, payload, found := strings.Cut(e.Output, "EVIDENCE "); found {
			payload = strings.TrimSpace(payload)
			if !json.Valid([]byte(payload)) {
				return fmt.Errorf("invalid scenario evidence: %s", payload)
			}
			s.Evidence = append(s.Evidence, json.RawMessage(payload))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	s.Passed, s.Failed, s.Skipped, s.Pending = &passed, &failed, &skipped, &pending
	s.CountSource = "go-test-events"
	return nil
}

func Generate(dir string) (Report, error) {
	r := Report{Status: "passed", GeneratedAt: time.Now().UTC().Format(time.RFC3339), RunDirectory: dir, GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH, CPUs: runtime.NumCPU()}
	b, err := os.ReadFile(filepath.Join(dir, "stages.tsv"))
	if err != nil {
		return r, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			return r, fmt.Errorf("invalid stage metadata %q", line)
		}
		code, err := strconv.Atoi(fields[1])
		if err != nil {
			return r, err
		}
		seconds, err := strconv.Atoi(fields[2])
		if err != nil {
			return r, err
		}
		s := Stage{Name: fields[0], ExitCode: code, Seconds: seconds}
		if s.Name == "engine-race" || s.Name == "engine-unit" || s.Name == "diagnostics" {
			if _, err := os.Stat(filepath.Join(dir, s.Name+".jsonl")); err != nil {
				s.Failures = append(s.Failures, "missing test event stream: "+err.Error())
			}
		}
		if s.Name == "operator-tests" {
			if err := parseGinkgo(filepath.Join(dir, "ginkgo.json"), &s); err != nil {
				_, requiredErr := os.Stat(filepath.Join(dir, "ginkgo-required"))
				if os.IsNotExist(err) && os.IsNotExist(requiredErr) {
					s.Notes = append(s.Notes, "Legacy run: Ginkgo spec counts were not collected; counts are N/A.")
				} else {
					s.Failures = append(s.Failures, "cannot collect Ginkgo evidence: "+err.Error())
				}
			}
		} else if err := parseEvents(filepath.Join(dir, s.Name+".jsonl"), &s); err != nil {
			s.Failures = append(s.Failures, "cannot parse test evidence: "+err.Error())
		}
		if s.Name == "performance" {
			r.Performance = collectPerformance(dir, &s)
		}
		if s.Name == "kind-smoke" {
			hasProbe := false
			if data, e := os.ReadFile(filepath.Join(dir, "probe.json")); e == nil {
				if !json.Valid(data) {
					s.Failures = append(s.Failures, "invalid probe evidence")
				} else {
					s.Evidence = append(s.Evidence, json.RawMessage(data))
					hasProbe = true
				}
			} else {
				s.Failures = append(s.Failures, "missing continuity probe evidence")
			}
			if data, e := os.ReadFile(filepath.Join(dir, "kind-smoke.log")); e == nil {
				for _, line := range strings.Split(string(data), "\n") {
					if strings.HasPrefix(line, "EVIDENCE ") {
						payload := strings.TrimPrefix(line, "EVIDENCE ")
						if json.Valid([]byte(payload)) {
							var fields map[string]any
							_ = json.Unmarshal([]byte(payload), &fields)
							if hasProbe && fields["environment"] != nil {
								continue
							}
							s.Evidence = append(s.Evidence, json.RawMessage(payload))
						}
					}
				}
			}
		}
		if code != 0 || (s.Failed != nil && *s.Failed != 0) || len(s.Failures) > 0 {
			r.Status = "failed"
		}
		r.Stages = append(r.Stages, s)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return r, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			r.Artifacts = append(r.Artifacts, e.Name())
		}
	}
	b, err = json.MarshalIndent(r, "", "  ")
	if err != nil {
		return r, err
	}
	if err = os.WriteFile(filepath.Join(dir, "report.json"), append(b, '\n'), 0644); err != nil {
		return r, err
	}
	var md strings.Builder
	fmt.Fprintf(&md, "# SimpleCache validation report\n\nStatus: **%s**\n\nGenerated: %s\n\nEnvironment: %s, %s, %d logical CPUs.\n\n", r.Status, r.GeneratedAt, r.GoVersion, r.Platform, r.CPUs)
	md.WriteString("| Stage | Exit | Seconds | Count source | Passed | Failed | Skipped | Pending |\n| --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, s := range r.Stages {
		source := s.CountSource
		if source == "" {
			source = "N/A"
		}
		fmt.Fprintf(&md, "| %s | %d | %d | %s | %s | %s | %s | %s |\n", s.Name, s.ExitCode, s.Seconds, source, countText(s.Passed), countText(s.Failed), countText(s.Skipped), countText(s.Pending))
	}
	if r.Performance != nil {
		renderPerformance(&md, *r.Performance)
	}
	for _, s := range r.Stages {
		fmt.Fprintf(&md, "\n## %s evidence\n\nRaw log: [%s.log](%s.log).\n\n", s.Name, s.Name, s.Name)
		for _, failure := range s.Failures {
			fmt.Fprintf(&md, "Failed: `%s`\n\n", failure)
		}
		for _, note := range s.Notes {
			fmt.Fprintf(&md, "%s\n\n", note)
		}
		for _, e := range s.Evidence {
			var pretty strings.Builder
			var v any
			_ = json.Unmarshal(e, &v)
			b, _ := json.MarshalIndent(v, "", "  ")
			pretty.Write(b)
			fmt.Fprintf(&md, "```json\n%s\n```\n\n", pretty.String())
		}
	}
	md.WriteString("## Interpretation\n\nLocal scenario tests emit functional-contract evidence. Their profiles cover the combined test process, including the client and assertions; they do not establish server throughput or resource baselines. Continuity probe timings include port-forward overhead and are diagnostic observations. Separate-process performance runs profile only the named server subprocess; their unprofiled baseline and profiled trials are reported separately. Both use loopback and synthetic sources, not deployed Pods. Source limits and SingleFlight are per process. Logical cache bytes are not RSS; removals include expiry. Reported timings are observations, not fixed performance gates. Kind uses one host and does not prove multi-machine availability. Aborted or interrupted stages are failures, not passes. N/A/null counts mean uncollected, not zero.\n")
	return r, os.WriteFile(filepath.Join(dir, "report.md"), []byte(md.String()), 0644)
}

func countText(n *int) string {
	if n == nil {
		return "N/A"
	}
	return strconv.Itoa(*n)
}
