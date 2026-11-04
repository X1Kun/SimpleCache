package measurement

import (
	"reflect"
	"testing"
)

func TestQuantilesNearestRankPreservesSamples(t *testing.T) {
	if got := Quantiles(nil); got != (Percentiles{}) {
		t.Fatalf("empty: %+v", got)
	}
	samples := []float64{4, 1, 3, 2}
	before := append([]float64(nil), samples...)
	if got := Quantiles(samples); got != (Percentiles{Count: 4, P50: 2, P95: 4, P99: 4}) {
		t.Fatalf("quantiles: %+v", got)
	}
	if !reflect.DeepEqual(samples, before) {
		t.Fatal("samples mutated")
	}
	if got := Quantiles([]float64{7}); got.P50 != 7 || got.P95 != 7 || got.P99 != 7 {
		t.Fatalf("singleton: %+v", got)
	}
}
