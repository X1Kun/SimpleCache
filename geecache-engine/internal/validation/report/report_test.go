package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReportsRetainFailuresAndEvidence(t *testing.T) {
	for _, code := range []string{"0", "1", "130"} {
		t.Run(code, func(t *testing.T) {
			dir := t.TempDir()
			if e := os.WriteFile(filepath.Join(dir, "stages.tsv"), []byte("scenario\t"+code+"\t1\n"), 0600); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(filepath.Join(dir, "scenario.jsonl"), []byte("{\"Action\":\"output\",\"Test\":\"TestX\",\"Output\":\"EVIDENCE {\\\"requests\\\":3}\\n\"}\n{\"Action\":\"pass\",\"Test\":\"TestX\"}\n"), 0600); e != nil {
				t.Fatal(e)
			}
			r, e := Generate(dir)
			if e != nil {
				t.Fatal(e)
			}
			want := "passed"
			if code != "0" {
				want = "failed"
			}
			if r.Status != want || len(r.Stages[0].Evidence) != 1 {
				t.Fatalf("%+v", r)
			}
			md, e := os.ReadFile(filepath.Join(dir, "report.md"))
			if e != nil || !strings.Contains(string(md), "requests") {
				t.Fatalf("report: %v", e)
			}
		})
	}
}

func TestMalformedEventsStillProduceFailureReport(t *testing.T) {
	dir := t.TempDir()
	if e := os.WriteFile(filepath.Join(dir, "stages.tsv"), []byte("engine-race\t0\t1\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "engine-race.jsonl"), []byte("truncated-event"), 0600); e != nil {
		t.Fatal(e)
	}
	r, e := Generate(dir)
	if e != nil {
		t.Fatal(e)
	}
	if r.Status != "failed" || len(r.Stages[0].Failures) == 0 {
		t.Fatalf("%+v", r)
	}
	if _, e = os.Stat(filepath.Join(dir, "report.md")); e != nil {
		t.Fatal(e)
	}
}

func TestKindProbeEvidenceIsNotDuplicated(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"stages.tsv":     "kind-smoke\t0\t1\n",
		"probe.json":     "{\"environment\":\"Kind\",\"requests\":3}",
		"kind-smoke.log": "EVIDENCE {\"environment\":\"Kind\",\"requests\":3}\nEVIDENCE {\"scaling_cycles\":2}\n",
	}
	for name, data := range files {
		if e := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); e != nil {
			t.Fatal(e)
		}
	}
	r, e := Generate(dir)
	if e != nil {
		t.Fatal(e)
	}
	if r.Status != "passed" || len(r.Stages[0].Evidence) != 2 {
		t.Fatalf("%+v", r)
	}
}
