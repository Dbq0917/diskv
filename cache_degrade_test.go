package diskv

import (
	"bytes"
	"fmt"
	"strconv"
	"sync"
	"testing"
)

// A value bigger than the whole cache must not panic or kill the process.
// It simply isn't cached, while smaller entries already in the cache stay
// put and keep being served from memory.
func TestCacheOversizedValueDegrades(t *testing.T) {
	d := New(Options{
		BasePath:     "test-cache-degrade",
		CacheSizeMax: 64,
	})
	defer d.EraseAll()

	smallKey := "small"
	smallVal := bytes.Repeat([]byte{'a'}, 32) // fits, fills half the cache
	if err := d.Write(smallKey, smallVal); err != nil {
		t.Fatalf("write small: %s", err)
	}
	if _, err := d.Read(smallKey); err != nil {
		t.Fatalf("read small: %s", err)
	}
	if !d.IsCached(smallKey) {
		t.Fatalf("small value should be cached")
	}

	bigKey := "big"
	bigVal := bytes.Repeat([]byte{'b'}, 256) // larger than CacheSizeMax
	if err := d.Write(bigKey, bigVal); err != nil {
		t.Fatalf("write big: %s", err)
	}

	// Reading the oversized value must succeed and return correct data,
	// without panicking, and must not evict the unrelated small value.
	got, err := d.Read(bigKey)
	if err != nil {
		t.Fatalf("read oversized value: %s", err)
	}
	if !bytes.Equal(got, bigVal) {
		t.Fatalf("oversized value mismatch")
	}
	if d.IsCached(bigKey) {
		t.Fatalf("oversized value should not be cached")
	}
	if !d.IsCached(smallKey) {
		t.Fatalf("small value was evicted while trying to cache an oversized value")
	}
	if d.CacheSize() > d.CacheSizeMax {
		t.Fatalf("cache size %d exceeds max %d", d.CacheSize(), d.CacheSizeMax)
	}

	// The oversized key goes back to disk on every read and stays correct.
	got2, err := d.Read(bigKey)
	if err != nil {
		t.Fatalf("second read oversized value: %s", err)
	}
	if !bytes.Equal(got2, bigVal) {
		t.Fatalf("second oversized read mismatch")
	}
	if d.IsCached(bigKey) {
		t.Fatalf("oversized value should never become cached")
	}
}

// With a small cache and many values that individually fit but jointly don't,
// concurrent reads must not panic, must not overshoot the cache budget, and
// must keep every read correct regardless of whether the key is cached.
func TestCacheConcurrentLargeValues(t *testing.T) {
	const (
		cacheMax  = 256
		keyCount  = 50
		valueSize = 64 // 4 values fit at most; the rest are evicted or skipped
		rounds    = 20
	)

	d := New(Options{
		BasePath:     "test-cache-concurrent",
		CacheSizeMax: cacheMax,
	})
	defer d.EraseAll()

	keys := make([]string, keyCount)
	values := make(map[string][]byte, keyCount)
	for i := 0; i < keyCount; i++ {
		key := "key-" + strconv.Itoa(i)
		val := bytes.Repeat([]byte{byte('A' + i%26)}, valueSize)
		keys[i] = key
		values[key] = val
		if err := d.Write(key, val); err != nil {
			t.Fatalf("write %s: %s", key, err)
		}
	}

	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		errMu   sync.Mutex
		readErr error
	)

	recordErr := func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if readErr == nil {
			readErr = err
		}
	}

	for r := 0; r < rounds; r++ {
		for _, key := range keys {
			wg.Add(1)
			go func(k string) {
				defer wg.Done()
				<-start
				got, err := d.Read(k)
				if err != nil {
					recordErr(err)
					return
				}
				if !bytes.Equal(got, values[k]) {
					recordErr(fmt.Errorf("value mismatch for %q", k))
				}
			}(key)
		}
	}
	close(start)
	wg.Wait()

	if readErr != nil {
		t.Fatalf("concurrent reads failed: %s", readErr)
	}

	// The process survived (we got here). Now verify the cache invariants.
	if d.CacheSize() > d.CacheSizeMax {
		t.Fatalf("cache size %d exceeds hard max %d", d.CacheSize(), d.CacheSizeMax)
	}

	var accounted uint64
	for _, key := range d.CachedKeys() {
		// Every reported key must really be a cache hit.
		if !d.IsCached(key) {
			t.Fatalf("CachedKeys reported %q but IsCached is false", key)
		}
		accounted += uint64(valueSize)
	}
	if accounted != d.CacheSize() {
		t.Fatalf("accounted cached bytes %d != CacheSize %d", accounted, d.CacheSize())
	}

	// A cached key is served from memory and stays correct; an uncached key
	// goes back to disk and is still correct. Both paths must agree on data.
	cached := map[string]bool{}
	for _, key := range d.CachedKeys() {
		cached[key] = true
	}
	for _, key := range keys {
		got, err := d.Read(key)
		if err != nil {
			t.Fatalf("final read %s: %s", key, err)
		}
		if !bytes.Equal(got, values[key]) {
			t.Fatalf("final read mismatch for %q (cached=%v)", key, cached[key])
		}
	}

	t.Logf("cache: %d/%d keys resident, %d/%d bytes used",
		len(cached), keyCount, d.CacheSize(), d.CacheSizeMax)
}

// Eviction must only remove other keys: when refilling a key already in the
// cache, its own entry must not be counted as evictable room, and the cache
// total must remain consistent.
func TestCacheEvictionPicksOtherKeys(t *testing.T) {
	d := New(Options{
		BasePath:     "test-cache-eviction",
		CacheSizeMax: 100,
	})
	defer d.EraseAll()

	k1, v1 := "k1", bytes.Repeat([]byte{'1'}, 40)
	k2, v2 := "k2", bytes.Repeat([]byte{'2'}, 40)
	k3, v3 := "k3", bytes.Repeat([]byte{'3'}, 40)

	for k, v := range map[string][]byte{k1: v1, k2: v2} {
		if err := d.Write(k, v); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Read(k); err != nil {
			t.Fatal(err)
		}
	}
	if d.CacheSize() != 80 {
		t.Fatalf("expected 80 bytes cached, got %d", d.CacheSize())
	}

	if err := d.Write(k3, v3); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Read(k3); err != nil {
		t.Fatal(err)
	}

	if d.CacheSize() > d.CacheSizeMax {
		t.Fatalf("cache size %d exceeds max %d", d.CacheSize(), d.CacheSizeMax)
	}
	// Exactly one of the three 40-byte values had to make room; two remain.
	if resident := len(d.CachedKeys()); resident != 2 {
		t.Fatalf("expected 2 resident keys after eviction, got %d", resident)
	}

	// Every key still reads correctly from whichever path (cache or disk).
	for k, want := range map[string][]byte{k1: v1, k2: v2, k3: v3} {
		got, err := d.Read(k)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("mismatch for %s", k)
		}
	}
}

// Even if the cache accounting is somehow inconsistent (no evictable entry
// but no room on paper), caching must degrade to "not cached" instead of
// panicking and killing the process. This is the old issue-#40 panic path.
func TestCacheNoRoomDegradesWithoutPanic(t *testing.T) {
	d := New(Options{
		BasePath:     "test-cache-noroom",
		CacheSizeMax: 100,
	})
	defer d.EraseAll()

	d.mu.Lock()
	d.cacheSize = 95 // phantom usage: empty map, but accounting says full
	d.mu.Unlock()

	key, val := "k", bytes.Repeat([]byte{'x'}, 20) // fits the max on its own
	if err := d.Write(key, val); err != nil {
		t.Fatal(err)
	}

	// Must not panic; the read still succeeds and returns correct data.
	got, err := d.Read(key)
	if err != nil {
		t.Fatalf("read failed: %s", err)
	}
	if !bytes.Equal(got, val) {
		t.Fatalf("read mismatch")
	}
	if d.IsCached(key) {
		t.Fatalf("value should not be cached when no room can be made")
	}
	if d.CacheSize() > d.CacheSizeMax {
		t.Fatalf("cache size %d exceeds max %d", d.CacheSize(), d.CacheSizeMax)
	}
}
