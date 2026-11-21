package diskv

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// failingReader returns fail after delivering okBytes bytes. It emulates the
// short/ENOSPC writes seen when a quota or the filesystem fills up midway
// through a batch job.
type failingReader struct {
	remaining int
	fail      error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, r.fail
	}
	n := len(p)
	if n > r.remaining {
		n = r.remaining
	}
	r.remaining -= n
	if r.remaining == 0 {
		return n, r.fail
	}
	return n, nil
}

// countDataFiles walks BasePath and returns the number of regular files that
// represent stored values.
func countDataFiles(t *testing.T, basePath string) int {
	t.Helper()
	count := 0
	err := filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %s", basePath, err)
	}
	return count
}

func indexLen(d *Diskv) int {
	return len(d.Index.Keys("", 1<<30))
}

// TestWriteFailureLeavesNoIndexEntry is the quota-exhaustion regression. When
// the on-disk write fails the key must not be reachable through the index,
// otherwise reads keep hitting a file that does not exist until the process
// is restarted and the index is rebuilt from disk.
func TestWriteFailureLeavesNoIndexEntry(t *testing.T) {
	cases := []struct {
		name    string
		tempDir string
		sync    bool
	}{
		{name: "direct", sync: false},
		{name: "direct_sync", sync: true},
		{name: "tempdir", tempDir: "test-quota-tmp", sync: false},
		{name: "tempdir_sync", tempDir: "test-quota-tmp", sync: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			basePath := "test-quota-" + tc.name
			opts := Options{
				BasePath:     basePath,
				TempDir:      tc.tempDir,
				CacheSizeMax: 1024,
				Index:        &BTreeIndex{},
				IndexLess:    strLess,
			}
			d := New(opts)
			defer func() {
				d.EraseAll()
				if tc.tempDir != "" {
					os.RemoveAll(tc.tempDir)
				}
			}()

			// A handful of writes that succeed, followed by writes that fail
			// mid-copy, exactly like a batch job hitting a full quota.
			const okWrites, failWrites = 5, 50
			for i := 0; i < okWrites; i++ {
				key := "ok-" + strconv.Itoa(i)
				if err := d.Write(key, []byte("payload")); err != nil {
					t.Fatalf("seed write %s: %s", key, err)
				}
			}

			for i := 0; i < failWrites; i++ {
				key := "fail-" + strconv.Itoa(i)
				r := &failingReader{remaining: 3, fail: syscall.ENOSPC}
				if err := d.WriteStream(key, r, tc.sync); err == nil {
					t.Fatalf("write %s unexpectedly succeeded", key)
				} else if d.isIndexed(key) {
					t.Fatalf("key %s indexed despite failed write", key)
				} else if d.Has(key) {
					t.Fatalf("key %s left on disk despite failed write", key)
				}
			}

			gotIndex, gotFiles := indexLen(d), countDataFiles(t, basePath)
			if gotIndex != okWrites {
				t.Fatalf("index entries = %d, want %d (failed writes must not be indexed)", gotIndex, okWrites)
			}
			if gotFiles != okWrites {
				t.Fatalf("data files = %d, want %d (failed writes must not leave files)", gotFiles, okWrites)
			}

			// Every indexed key must be readable; this is the invariant that
			// was broken before the fix.
			for _, key := range d.Index.Keys("", 1<<30) {
				if _, err := d.Read(key); err != nil {
					t.Fatalf("indexed key %s is not readable: %s", key, err)
				}
			}

			// A subsequent healthy write still works and is indexed, proving
			// the store recovers without a restart.
			if err := d.Write("recovered", []byte("payload")); err != nil {
				t.Fatalf("recovery write: %s", err)
			}
			if !d.isIndexed("recovered") {
				t.Fatalf("recovered key not indexed")
			}
			if got := countDataFiles(t, basePath); got != okWrites+1 {
				t.Fatalf("data files after recovery = %d, want %d", got, okWrites+1)
			}
		})
	}
}

// TestWriteFailureAtEveryStage exercises each stage that can return an error
// once the key file already exists, ensuring none of them indexes the key.
func TestWriteFailureAtEveryStage(t *testing.T) {
	basePath := "test-quota-stages"
	d := New(Options{
		BasePath:     basePath,
		CacheSizeMax: 1024,
		Index:        &BTreeIndex{},
		IndexLess:    strLess,
	})
	defer d.EraseAll()

	stages := []struct {
		name string
		r    *failingReader
		sync bool
	}{
		{"copy_immediate", &failingReader{remaining: 0, fail: syscall.ENOSPC}, false},
		{"copy_partial", &failingReader{remaining: 7, fail: syscall.EDQUOT}, false},
		{"sync", &failingReader{remaining: 4, fail: syscall.ENOSPC}, true},
	}

	for i, st := range stages {
		key := "stage-" + st.name
		if err := d.WriteStream(key, st.r, st.sync); err == nil {
			t.Fatalf("stage %d (%s): expected failure", i, st.name)
		}
		if d.isIndexed(key) {
			t.Fatalf("stage %s indexed the key after failure", st.name)
		}
	}

	if got := indexLen(d); got != 0 {
		t.Fatalf("index entries = %d, want 0", got)
	}
	if got := countDataFiles(t, basePath); got != 0 {
		t.Fatalf("data files = %d, want 0", got)
	}

	// TempDir leftovers must be cleaned up as well.
	if leftovers, err := ioutil.ReadDir(basePath); err == nil && len(leftovers) != 0 {
		t.Fatalf("base path contains %d leftover entries", len(leftovers))
	}
}

// TestOverwriteFailureNoDuplicateIndex covers an existing key whose rewrite
// fails when the quota is exhausted. Even though the rewrite errors out, the
// index must never gain a phantom entry and the key still resolves to a
// single on-disk path rather than dangling forever.
func TestOverwriteFailureNoDuplicateIndex(t *testing.T) {
	basePath := "test-quota-overwrite-tmp"
	d := New(Options{
		BasePath:     basePath,
		TempDir:      "test-quota-overwrite-tmpdir",
		CacheSizeMax: 1024,
		Index:        &BTreeIndex{},
		IndexLess:    strLess,
	})
	defer d.EraseAll()
	defer os.RemoveAll("test-quota-overwrite-tmpdir")

	key := "existing"
	original := []byte("original-value")
	if err := d.Write(key, original); err != nil {
		t.Fatalf("initial write: %s", err)
	}

	r := &failingReader{remaining: 4, fail: syscall.ENOSPC}
	if err := d.WriteStream(key, r, true); err == nil {
		t.Fatal("expected overwrite to fail")
	}

	if !d.isIndexed(key) {
		t.Fatal("existing key vanished from index after failed overwrite")
	}
	got, err := d.Read(key)
	if err != nil {
		t.Fatalf("key no longer readable after failed overwrite: %s", err)
	}
	if string(got) != string(original) {
		t.Fatalf("value changed to %q after failed overwrite", string(got))
	}
	if n := indexLen(d); n != 1 {
		t.Fatalf("index entries = %d, want 1", n)
	}
	if n := countDataFiles(t, basePath); n != 1 {
		t.Fatalf("data files = %d, want 1", n)
	}
}

// TestBatchQuotaExhaustion mirrors the nightly batch: tens of thousands of
// writes where a large suffix fails with ENOSPC. Index length and on-disk
// file count must stay identical throughout.
func TestBatchQuotaExhaustion(t *testing.T) {
	basePath := "test-quota-batch"
	d := New(Options{
		BasePath:     basePath,
		CacheSizeMax: 0,
		Index:        &BTreeIndex{},
		IndexLess:    strLess,
	})
	defer d.EraseAll()

	const total = 20000
	const capacity = 137
	for i := 0; i < total; i++ {
		key := "batch-" + strconv.Itoa(i)
		var err error
		if i < capacity {
			err = d.Write(key, []byte("x"))
		} else {
			err = d.WriteStream(key, &failingReader{remaining: 1, fail: syscall.ENOSPC}, false)
		}
		if i < capacity && err != nil {
			t.Fatalf("write %d failed early: %s", i, err)
		}
		if i >= capacity && err == nil {
			t.Fatalf("write %d should have failed", i)
		}
	}

	if got := indexLen(d); got != capacity {
		t.Fatalf("index entries = %d, want %d", got, capacity)
	}
	if got := countDataFiles(t, basePath); got != capacity {
		t.Fatalf("data files = %d, want %d", got, capacity)
	}

	// Reopening the store rebuilds the index from disk; the rebuilt index
	// must match what the live process holds, i.e. no phantom keys survive
	// a restart either.
	d2 := New(Options{
		BasePath:     basePath,
		CacheSizeMax: 0,
		Index:        &BTreeIndex{},
		IndexLess:    strLess,
	})
	defer d2.EraseAll()
	if got := indexLen(d2); got != capacity {
		t.Fatalf("rebuilt index entries = %d, want %d", got, capacity)
	}
}
