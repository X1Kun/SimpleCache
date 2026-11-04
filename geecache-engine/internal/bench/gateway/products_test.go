package gateway

import (
	"bytes"
	"strings"
	"testing"
)

func TestDatasetRejectsEncodedContentOverflow(t *testing.T) {
	c := config("http://localhost:8000")
	c.Prefix = strings.Repeat("<", 80)
	c.DatasetProfile = "mixed"
	c.KeyStyle = "mixed"
	if err := Validate(c); err == nil {
		t.Fatal("accepted overflowing JSON header")
	}
	if _, err := DescribeDataset(c); err == nil {
		t.Fatal("manifest accepted overflowing header")
	}
	var sql bytes.Buffer
	if err := WriteFixture(&sql, c); err == nil || sql.Len() != 0 {
		t.Fatal("invalid fixture emitted SQL", err)
	}
	c = config("http://localhost:8000")
	c.Scenario = "arrival"
	if Validate(c) == nil {
		t.Fatal("arrival without stages accepted")
	}
}
func TestMixedDatasetKeepsExactEncodedSizes(t *testing.T) {
	c := config("http://localhost:8000")
	c.Keys = 200
	c.DatasetProfile = "mixed"
	c.KeyStyle = "mixed"
	if err := Validate(c); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < c.Keys; i++ {
		for _, v := range []int64{1, 9223372036854775807} {
			value, err := c.productValue(i, v)
			size, _ := c.sizeClass(i)
			if err != nil || len(value) != size {
				t.Fatal(i, v, len(value), size, err)
			}
		}
	}
}
