package diskv

import (
	"os"
	"testing"
)

// Reproduce: an external script deletes files out from under the store while
// the index still holds the keys. Reads should behave like a cache miss
// (a clean, checkable not-exist error), not leak a raw syscall error.
func TestExternalDeleteRead(t *testing.T) {
	d := New(Options{
		BasePath:     "test-external-delete",
		CacheSizeMax: 1024,
		Index:        &BTreeIndex{},
		IndexLess:    func(a, b string) bool { return a < b },
	})
	defer d.EraseAll()

	keys := []string{"alpha", "beta", "gamma"}
	for _, k := range keys {
		if err := d.WriteString(k, "value-"+k); err != nil {
			t.Fatal(err)
		}
	}

	// External cleanup script removes the underlying files directly.
	for _, k := range keys {
		pathKey := d.transform(k)
		if err := os.Remove(d.completeFilename(pathKey)); err != nil {
			t.Fatal(err)
		}
	}

	for _, k := range keys {
		_, err := d.Read(k)
		t.Logf("Read(%q) after external delete: err=%v (IsNotExist=%v)", k, err, os.IsNotExist(err))
		if err == nil {
			t.Fatalf("Read(%q): expected error after external delete, got nil", k)
		}
		if !os.IsNotExist(err) {
			t.Fatalf("Read(%q): expected a not-exist error, got %T: %v", k, err, err)
		}
	}

	// The index should have been lazily repaired: stale keys removed.
	if got := d.Index.Keys("", len(keys)); len(got) != 0 {
		t.Fatalf("index still holds stale keys after read misses: %v", got)
	}
}
