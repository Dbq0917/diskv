package diskv

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestScaleMillionKeys compares the legacy flat layout against two fixed-depth
// prefix layouts on a large, image-cache-like dataset. It is skipped by default
// because it materializes around one million small files per layout; opt in
// with:
//
//	DISKV_SCALE_KEYS=1000000 go test -run TestScaleMillionKeys -v -timeout 2h
//
// Optional knobs:
//
//	DISKV_SCALE_VALUE_BYTES  value size per key (default 32768)
//	DISKV_SCALE_PREFIX      length of the prefix used for the prefix-listing
//	                        measurement (default 4)
func TestScaleMillionKeys(t *testing.T) {
	n, err := strconv.Atoi(os.Getenv("DISKV_SCALE_KEYS"))
	if err != nil || n <= 0 {
		t.Skip("set DISKV_SCALE_KEYS to a positive count to run the scale test")
	}

	valueSize := 32 * 1024
	if raw := os.Getenv("DISKV_SCALE_VALUE_BYTES"); raw != "" {
		if parsed, perr := strconv.Atoi(raw); perr == nil && parsed > 0 {
			valueSize = parsed
		}
	}

	prefixLen := 4
	if raw := os.Getenv("DISKV_SCALE_PREFIX"); raw != "" {
		if parsed, perr := strconv.Atoi(raw); perr == nil && parsed > 0 {
			prefixLen = parsed
		}
	}

	value := make([]byte, valueSize)
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		// LCG over the full 64-bit space so leading hex digits (and therefore
		// shards) are spread uniformly, instead of clustering on "0000...".
		keys[i] = fmt.Sprintf("%016x", scaleLCG(uint64(i+1)))
	}

	prefix := keys[0][:prefixLen]
	prefixHits := 0
	for _, key := range keys {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			prefixHits++
		}
	}
	t.Logf("prefix %q matches %d of %d keys", prefix, prefixHits, n)

	t.Run("flat", func(t *testing.T) {
		d := New(Options{BasePath: "scale-flat"})
		measureLayout(t, d, nil, "scale-flat", keys, value, prefix)
	})

	// The single-level layout (depth 1) gives 16^2 = 256 leaf directories of
	// ~3900 keys each. The two-level layout (depth 2) allows 16^4 = 65536
	// leaves, which over-shards one million keys into mostly-empty
	// directories and shows the cost of choosing a leaf capacity close to (or
	// above) the number of keys.
	for _, tc := range []struct {
		name  string
		depth int
	}{
		{"sharded-depth1", 1},
		{"sharded-depth2", 2},
	} {
		tc := tc
		name := tc.name
		t.Run(name, func(t *testing.T) {
			layout, err := NewPrefixLayout("0123456789abcdef", 2, tc.depth, "_")
			if err != nil {
				t.Fatal(err)
			}
			d := New(Options{
				BasePath:          "scale-" + name,
				AdvancedTransform: layout.AdvancedTransform,
				InverseTransform:  layout.InverseTransform,
				PrefixLocator:     layout,
			})
			measureLayout(t, d, layout, "scale-"+name, keys, value, prefix)
		})
	}
}

// scaleLCG is the MMIX 64-bit linear congruential generator (Knuth), which
// permutes its inputs: distinct seeds produce distinct outputs.
func scaleLCG(seed uint64) uint64 {
	const (
		multiplier = 6364136223846793005
		increment  = 1442695040888963407
	)
	return seed*multiplier + increment
}

func measureLayout(t *testing.T, d *Diskv, layout *PrefixLayout, basePath string, keys []string, value []byte, prefix string) {
	t.Helper()

	writeStart := time.Now()
	for i, key := range keys {
		if err := d.Write(key, value); err != nil {
			t.Fatalf("write #%d: %s", i, err)
		}
	}
	t.Logf("write %d keys: %s (%.0f keys/s)",
		len(keys), time.Since(writeStart),
		float64(len(keys))/time.Since(writeStart).Seconds())

	dirs, leafFiles, maxLeaf := countLayout(t, basePath)
	t.Logf("on-disk layout: %d dirs, %d data files, largest dir holds %d entries",
		dirs, leafFiles, maxLeaf)

	listStart := time.Now()
	listed := drainKeys(d.Keys(nil))
	t.Logf("list all %d keys: %s", listed, time.Since(listStart))
	if listed != len(keys) {
		t.Fatalf("listed %d keys, want %d", listed, len(keys))
	}

	prefixStart := time.Now()
	prefixCount := drainKeys(d.KeysPrefix(prefix, nil))
	t.Logf("list prefix %q (%d keys): %s", prefix, prefixCount, time.Since(prefixStart))

	if layout != nil {
		wantPrefix := 0
		for _, key := range keys {
			if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
				wantPrefix++
			}
		}
		if prefixCount != wantPrefix {
			t.Fatalf("prefix count = %d, want %d", prefixCount, wantPrefix)
		}
	}

	eraseStart := time.Now()
	if err := d.EraseAll(); err != nil {
		t.Fatalf("EraseAll: %s", err)
	}
	t.Logf("erase all: %s", time.Since(eraseStart))
}

func drainKeys(c <-chan string) int {
	count := 0
	for range c {
		count++
	}
	return count
}

func countLayout(t *testing.T, root string) (dirs, files, maxLeaf int) {
	t.Helper()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files++
			return nil
		}

		dirs++
		entries, err := ioutil.ReadDir(path)
		if err != nil {
			return err
		}

		entryCount := len(entries)
		if entryCount > maxLeaf {
			maxLeaf = entryCount
		}
		return nil
	})
	if err != nil {
		t.Fatalf("count layout: %s", err)
	}
	return dirs, files, maxLeaf
}
