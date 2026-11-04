package gateway

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"io"
	"strings"
)

const maxDatasetBytes int64 = 512 << 20

type DatasetManifest struct {
	Keys         int            `json:"keys"`
	KeyBytes     int64          `json:"key_bytes"`
	ValueBytes   int64          `json:"value_bytes"`
	LogicalBytes int64          `json:"logical_bytes"`
	Sizes        Percentiles    `json:"value_size_bytes"`
	Classes      map[string]int `json:"classes"`
	SHA256       string         `json:"sha256"`
}

func (c Config) datasetKey(index int) string {
	if c.KeyStyle != "mixed" {
		return Key(c.Prefix, index)
	}
	switch index % 4 {
	case 1:
		return fmt.Sprintf("%s-商品:%04d", c.Prefix, index)
	case 2:
		base := Key(c.Prefix, index)
		return base + strings.Repeat("x", 200-len(base))
	case 3:
		return fmt.Sprintf("%s-特殊 a+b/c?&%%-%04d", c.Prefix, index)
	default:
		return Key(c.Prefix, index)
	}
}
func (c Config) sizeClass(index int) (int, string) {
	if c.DatasetProfile != "mixed" {
		return c.ValueBytes, "uniform"
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", c.Seed, index)))
	bucket := (index + int((c.Seed%100+100)%100)) % 100
	sizes, class := []int{512, 1024, 2048}, "small"
	if bucket >= 70 {
		sizes, class = []int{4096, 8192, 16384}, "medium"
	}
	if bucket >= 95 {
		sizes, class = []int{32768, 49152, 65536}, "large"
	}
	return sizes[int(sum[0])%len(sizes)], class
}
func (c Config) productValue(index int, version int64) ([]byte, error) {
	key := c.datasetKey(index)
	size, class := c.sizeClass(index)
	if c.DatasetProfile != "mixed" {
		value := Value(key, version, size)
		if len(value) != size {
			return nil, fmt.Errorf("value size cannot hold encoded content")
		}
		return value, nil
	}
	product := struct {
		Key         string              `json:"key"`
		Version     int64               `json:"version"`
		Kind        string              `json:"kind"`
		Name        string              `json:"name"`
		Price       int                 `json:"price_cents"`
		Tags        []string            `json:"tags,omitempty"`
		Specs       map[string][]string `json:"specifications,omitempty"`
		Description string              `json:"description,omitempty"`
		Padding     string              `json:"padding"`
	}{Key: key, Version: version, Kind: class, Name: fmt.Sprintf("商品-%d", index), Price: 100 + index%100000}
	if class != "small" {
		product.Tags = []string{"可重复数据", "多规格"}
		product.Specs = map[string][]string{"颜色": {"红色", "蓝色"}, "尺寸": {"S", "M", "L"}}
	}
	if class == "large" {
		product.Description = strings.Repeat("这是用于验证变长商品详情的描述。", 30)
	}
	base, _ := json.Marshal(product)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s:%d", c.Seed, key, version)))
	padding := hex.EncodeToString(sum[:])
	needed := size - len(base)
	if needed < 0 {
		return nil, fmt.Errorf("value size cannot hold encoded content")
	}
	product.Padding = strings.Repeat(padding, (needed+len(padding)-1)/len(padding))[:needed]
	value, _ := json.Marshal(product)
	return value, nil
}
func validateDataset(c Config) error {
	if c.Prefix == "" || len(c.Prefix) > 80 || c.Keys < 1 || c.Keys > 50000 || c.ValueBytes < 128 || c.ValueBytes > 1<<20 {
		return fmt.Errorf("invalid dataset dimensions")
	}
	if c.DatasetProfile != "" && c.DatasetProfile != "uniform" && c.DatasetProfile != "mixed" {
		return fmt.Errorf("invalid dataset profile")
	}
	if c.KeyStyle != "" && c.KeyStyle != "plain" && c.KeyStyle != "mixed" {
		return fmt.Errorf("invalid key style")
	}
	var total int64
	for i := 0; i < c.Keys; i++ {
		key := c.datasetKey(i)
		size, _ := c.sizeClass(i)
		if err := cache.ValidateKey(key); err != nil {
			return fmt.Errorf("invalid dataset key: %w", err)
		}
		total += int64(len(key) + size)
	}
	if total > maxDatasetBytes {
		return fmt.Errorf("dataset exceeds 512MiB logical bound")
	}
	// Even uniform values must fit their JSON header for the longest generated key.
	for i := 0; i < c.Keys; i++ {
		if _, err := c.productValue(i, 1); err != nil {
			return fmt.Errorf("dataset key %d: %w", i, err)
		}
	}
	return nil
}

// DescribeDataset hashes length-prefixed UTF-8 keys/full values without retaining
// the entire working set. Both SQL seeding and request checking use this code.
func DescribeDataset(c Config) (DatasetManifest, error) {
	if err := validateDataset(c); err != nil {
		return DatasetManifest{}, err
	}
	manifest := DatasetManifest{Keys: c.Keys, Classes: map[string]int{}}
	sizes := make([]float64, 0, c.Keys)
	hash := sha256.New()
	var length [8]byte
	for i := 0; i < c.Keys; i++ {
		key := c.datasetKey(i)
		value, err := c.productValue(i, 1)
		if err != nil {
			return DatasetManifest{}, err
		}
		_, class := c.sizeClass(i)
		manifest.Classes[class]++
		manifest.KeyBytes += int64(len(key))
		manifest.ValueBytes += int64(len(value))
		sizes = append(sizes, float64(len(value)))
		for _, part := range [][]byte{[]byte(key), value} {
			binary.BigEndian.PutUint64(length[:], uint64(len(part)))
			_, _ = hash.Write(length[:])
			_, _ = hash.Write(part)
		}
	}
	manifest.LogicalBytes = manifest.KeyBytes + manifest.ValueBytes
	manifest.Sizes = Quantiles(sizes)
	manifest.SHA256 = hex.EncodeToString(hash.Sum(nil))
	return manifest, nil
}

// WriteFixture streams owned fixture rows. Call only while traffic/old Outbox
// events are quiescent, then replace cache processes before measuring.
func WriteFixture(w io.Writer, c Config) error {
	if err := validateDataset(c); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "SET standard_conforming_strings=on; BEGIN;"); err != nil {
		return err
	}
	for i := 0; i < c.Keys; i++ {
		key := strings.ReplaceAll(c.datasetKey(i), "'", "''")
		payload, err := c.productValue(i, 1)
		if err != nil {
			return err
		}
		value := hex.EncodeToString(payload)
		if _, err := fmt.Fprintf(w, "INSERT INTO simplecache_products(key,value,version,deleted) VALUES ('%s',decode('%s','hex'),1,false) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,version=1,deleted=false;\n", key, value); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w, "COMMIT;")
	return err
}

func (c Config) maxValueSize() int {
	if c.DatasetProfile == "mixed" {
		return 65536
	}
	return c.ValueBytes
}
