package diskv

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"
)

func strLess(a, b string) bool { return a < b }

func cmpStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (d *Diskv) isIndexed(key string) bool {
	if d.Index == nil {
		return false
	}

	for _, got := range d.Index.Keys("", 1000) {
		if got == key {
			return true
		}
	}
	return false
}

func TestIndexOrder(t *testing.T) {
	d := New(Options{
		BasePath:     "index-test",
		CacheSizeMax: 1024,
		Index:        &BTreeIndex{},
		IndexLess:    strLess,
	})
	defer d.EraseAll()

	v := []byte{'1', '2', '3'}
	d.Write("a", v)
	if !d.isIndexed("a") {
		t.Fatalf("'a' not indexed after write")
	}
	d.Write("1", v)
	d.Write("m", v)
	d.Write("-", v)
	d.Write("A", v)

	expectedKeys := []string{"-", "1", "A", "a", "m"}
	keys := []string{}
	for _, key := range d.Index.Keys("", 100) {
		keys = append(keys, key)
	}

	if !cmpStrings(keys, expectedKeys) {
		t.Fatalf("got %s, expected %s", keys, expectedKeys)
	}
}

func TestIndexLoad(t *testing.T) {
	d1 := New(Options{
		BasePath:     "index-test",
		CacheSizeMax: 1024,
	})
	defer d1.EraseAll()

	val := []byte{'1', '2', '3'}
	keys := []string{"a", "b", "c", "d", "e", "f", "g"}
	for _, key := range keys {
		d1.Write(key, val)
	}

	d2 := New(Options{
		BasePath:     "index-test",
		CacheSizeMax: 1024,
		Index:        &BTreeIndex{},
		IndexLess:    strLess,
	})
	defer d2.EraseAll()

	// check d2 has properly loaded existing d1 data
	for _, key := range keys {
		if !d2.isIndexed(key) {
			t.Fatalf("key '%s' not indexed on secondary", key)
		}
	}

	// cache one
	if readValue, err := d2.Read(keys[0]); err != nil {
		t.Fatalf("%s", err)
	} else if bytes.Compare(val, readValue) != 0 {
		t.Fatalf("%s: got %s, expected %s", keys[0], readValue, val)
	}

	// make sure it got cached
	for i := 0; i < 10 && !d2.isCached(keys[0]); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !d2.isCached(keys[0]) {
		t.Fatalf("key '%s' not cached", keys[0])
	}

	// kill the disk
	d1.EraseAll()

	// cached value should still be there in the second
	if readValue, err := d2.Read(keys[0]); err != nil {
		t.Fatalf("%s", err)
	} else if bytes.Compare(val, readValue) != 0 {
		t.Fatalf("%s: got %s, expected %s", keys[0], readValue, val)
	}

	// but not in the original
	if _, err := d1.Read(keys[0]); err == nil {
		t.Fatalf("expected error reading from flushed store")
	}
}

func TestIndexKeysEmptyFrom(t *testing.T) {
	d := New(Options{
		BasePath:     "index-test",
		CacheSizeMax: 1024,
		Index:        &BTreeIndex{},
		IndexLess:    strLess,
	})
	defer d.EraseAll()

	for _, k := range []string{"a", "c", "z", "b", "x", "b", "y"} {
		d.Write(k, []byte("1"))
	}

	want := []string{"a", "b", "c", "x", "y", "z"}
	have := d.Index.Keys("", 99)
	if !reflect.DeepEqual(want, have) {
		t.Errorf("want %v, have %v", want, have)
	}
}

func TestEraseAllIndexConsistency(t *testing.T) {
	keys := []string{"a", "b", "c"}
	newIndexedStore := func(basePath string) *Diskv {
		d := New(Options{
			BasePath:     basePath,
			CacheSizeMax: 1024,
			Index:        &BTreeIndex{},
			IndexLess:    strLess,
		})
		for _, key := range keys {
			if err := d.Write(key, []byte(key)); err != nil {
				t.Fatalf("write %q: %s", key, err)
			}
			if _, err := d.Read(key); err != nil {
				t.Fatalf("read %q: %s", key, err)
			}
		}
		return d
	}

	assertEmpty := func(label string, d *Diskv) {
		t.Helper()

		d.mu.RLock()
		cacheLen, cacheSize := len(d.cache), d.cacheSize
		d.mu.RUnlock()
		if cacheLen != 0 || cacheSize != 0 {
			t.Fatalf("%s: cache has %d keys (%d bytes), want empty", label, cacheLen, cacheSize)
		}

		if got := d.Index.Keys("", len(keys)+1); len(got) != 0 {
			t.Fatalf("%s: index has %d keys %v, want empty", label, len(got), got)
		}

		for key := range d.Keys(nil) {
			t.Fatalf("%s: disk still has key %q", label, key)
		}
	}

	cleared := newIndexedStore("index-erase-all-clear-test")
	defer cleared.EraseAll()
	if err := cleared.EraseAll(); err != nil {
		t.Fatalf("erase all: %s", err)
	}
	assertEmpty("EraseAll", cleared)

	erased := newIndexedStore("index-erase-all-delete-test")
	defer erased.EraseAll()
	for _, key := range keys {
		if err := erased.Erase(key); err != nil {
			t.Fatalf("erase %q: %s", key, err)
		}
	}
	assertEmpty("Erase each key", erased)
}

func TestEraseAllIndexPages(t *testing.T) {
	d := New(Options{
		BasePath:  "index-erase-all-pages-test",
		Index:     &BTreeIndex{},
		IndexLess: strLess,
	})
	defer d.EraseAll()

	const n = 2500
	for i := 0; i < n; i++ {
		d.Index.Insert(fmt.Sprintf("key-%04d", i))
	}
	if got := len(d.Index.Keys("", n+1)); got != n {
		t.Fatalf("index has %d keys before EraseAll, want %d", got, n)
	}

	if err := d.EraseAll(); err != nil {
		t.Fatalf("erase all: %s", err)
	}
	if got := len(d.Index.Keys("", n+1)); got != 0 {
		t.Fatalf("index has %d keys after EraseAll, want 0", got)
	}
}

func TestReadMissInvalidatesIndex(t *testing.T) {
	d := New(Options{
		BasePath:     "index-read-miss-test",
		CacheSizeMax: 1024,
		Index:        &BTreeIndex{},
		IndexLess:    strLess,
	})
	defer d.EraseAll()

	key := "gone"
	if err := d.Write(key, []byte("value")); err != nil {
		t.Fatalf("write: %s", err)
	}
	if err := os.Remove(d.completeFilename(d.transform(key))); err != nil {
		t.Fatalf("remove backing file: %s", err)
	}

	if _, err := d.Read(key); !os.IsNotExist(err) {
		t.Fatalf("read after external delete: got %v, want not-exist", err)
	}
	if d.isIndexed(key) {
		t.Fatalf("read miss left %q in the index", key)
	}
}

func TestBadKeys(t *testing.T) {
	d := New(Options{
		BasePath:     "index-test",
		CacheSizeMax: 1024,
		Index:        &BTreeIndex{},
		IndexLess:    strLess,
	})
	defer d.EraseAll()

	for _, k := range []string{"a/a"} {
		err := d.Write(k, []byte("1"))
		if err != errBadKey {
			t.Errorf("Expected bad key error, got: %v", err)
		}
	}
}
