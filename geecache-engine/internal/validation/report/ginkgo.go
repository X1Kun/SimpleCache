package report

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Decode the stable report fields without adding Ginkgo to the engine module.
type ginkgoSuite struct {
	SuiteDescription string
	SuiteSucceeded   *bool
	SpecReports      []struct {
		LeafNodeType            string
		LeafNodeText            string
		ContainerHierarchyTexts []string
		State                   string
		Failure                 struct{ Message string }
	}
}

func parseGinkgo(path string, s *Stage) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var suites []ginkgoSuite
	if err = json.Unmarshal(b, &suites); err != nil {
		return err
	}
	if len(suites) == 0 {
		return fmt.Errorf("empty Ginkgo suite report")
	}
	passed, failed, skipped, pending := 0, 0, 0, 0
	var failures []string
	for _, suite := range suites {
		if suite.SuiteSucceeded == nil {
			return fmt.Errorf("missing SuiteSucceeded")
		}
		if !*suite.SuiteSucceeded {
			failures = append(failures, "Ginkgo suite failed: "+suite.SuiteDescription)
		}
		if suite.SpecReports == nil {
			return fmt.Errorf("missing SpecReports")
		}
		for _, spec := range suite.SpecReports {
			if spec.LeafNodeType == "" {
				return fmt.Errorf("missing LeafNodeType")
			}
			// Suite hooks may fail but must not inflate the actual It counts.
			isFailure := false
			switch spec.State {
			case "passed", "skipped", "pending":
			case "failed", "timedout", "panicked", "interrupted", "aborted":
				isFailure = true
			default:
				return fmt.Errorf("unknown Ginkgo spec state %q", spec.State)
			}
			if isFailure {
				name := strings.Join(append(append([]string(nil), spec.ContainerHierarchyTexts...), spec.LeafNodeText), " / ")
				failures = append(failures, fmt.Sprintf("%s: %s (%s): %s", suite.SuiteDescription, name, spec.State, spec.Failure.Message))
			}
			if spec.LeafNodeType != "It" {
				continue
			}
			switch spec.State {
			case "passed":
				passed++
			case "skipped":
				skipped++
			case "pending":
				pending++
			default:
				failed++
			}
		}
	}
	s.Passed, s.Failed, s.Skipped, s.Pending = &passed, &failed, &skipped, &pending
	s.CountSource = "ginkgo-specs"
	s.Failures = append(s.Failures, failures...)
	return nil
}
