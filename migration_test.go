package diskv

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestMigrateReadsLatestValueAndCleansOldBasePath(t *testing.T) {
	oldBasePath := filepath.Join(t.TempDir(), "old")
	newBasePath := filepath.Join(t.TempDir(), "new")

	d := New(Options{
		BasePath:     oldBasePath,
		Transform:    func(key string) []string { return []string{key[:4]} },
		CacheSizeMax: 1024,
		Index:        &BTreeIndex{},
		IndexLess:    strLess,
	})
	defer d.EraseAll()

	const keyCount = 20
	values := make(map[string]string, keyCount)
	for i := 0; i < keyCount; i++ {
		key := fmt.Sprintf("key-%02d", i)
		value := fmt.Sprintf("old-value-%02d", i)
		if err := d.Write(key, []byte(value)); err != nil {
			t.Fatal(err)
		}
		values[key] = value
	}

	started := make(chan struct{})
	release := make(chan struct{})
	d.migrationHook = func() {
		close(started)
		<-release
	}

	migrated := make(chan error, 1)
	go func() {
		migrated <- d.Migrate(newBasePath)
	}()
	<-started

	// Before files move, reads still need to resolve through the old base path.
	for key, value := range values {
		got, err := d.Read(key)
		if err != nil {
			t.Fatalf("read %q during migration: %v", key, err)
		}
		if string(got) != value {
			t.Fatalf("read %q during migration: got %q, want %q", key, got, value)
		}
	}

	// Writes go to the new base path immediately and must win over old copies.
	for i := 0; i < keyCount/2; i++ {
		key := fmt.Sprintf("key-%02d", i)
		value := fmt.Sprintf("new-value-%02d", i)
		if err := d.Write(key, []byte(value)); err != nil {
			t.Fatal(err)
		}
		values[key] = value
	}
	if err := d.Write("new-key", []byte("new-key-value")); err != nil {
		t.Fatal(err)
	}
	values["new-key"] = "new-key-value"

	for key, value := range values {
		got, err := d.Read(key)
		if err != nil {
			t.Fatalf("read %q after update: %v", key, err)
		}
		if string(got) != value {
			t.Fatalf("read %q after update: got %q, want %q", key, got, value)
		}
	}

	seenKeys := map[string]bool{}
	for key := range d.Keys(nil) {
		if seenKeys[key] {
			t.Fatalf("key %q returned more than once during migration", key)
		}
		seenKeys[key] = true
	}
	for key := range values {
		if !seenKeys[key] {
			t.Fatalf("key %q is missing while migrating", key)
		}
	}

	close(release)
	if err := <-migrated; err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if _, err := os.Stat(oldBasePath); !os.IsNotExist(err) {
		t.Fatalf("old base path still exists: %v", err)
	}
	for key, value := range values {
		got, err := d.Read(key)
		if err != nil {
			t.Fatalf("read %q after migration: %v", key, err)
		}
		if string(got) != value {
			t.Fatalf("read %q after migration: got %q, want %q", key, got, value)
		}
		if !d.isIndexed(key) {
			t.Fatalf("%q is missing from the index after migration", key)
		}
	}
}

func TestMigrateDoesNotCacheValueReadFromOldBasePath(t *testing.T) {
	oldBasePath := filepath.Join(t.TempDir(), "old")
	newBasePath := filepath.Join(t.TempDir(), "new")

	d := New(Options{
		BasePath:     oldBasePath,
		Transform:    func(string) []string { return nil },
		CacheSizeMax: 1024,
	})
	defer d.EraseAll()

	const key = "key"
	if err := d.Write(key, []byte("old-value")); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	d.migrationHook = func() {
		close(started)
		<-release
	}

	migrated := make(chan error, 1)
	go func() {
		migrated <- d.Migrate(newBasePath)
	}()
	<-started

	oldReader, err := d.ReadStream(key, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Write(key, []byte("new-value")); err != nil {
		t.Fatal(err)
	}
	if _, err := ioutil.ReadAll(oldReader); err != nil {
		t.Fatal(err)
	}
	if err := oldReader.Close(); err != nil {
		t.Fatal(err)
	}

	close(release)
	if err := <-migrated; err != nil {
		t.Fatalf("migrate: %v", err)
	}

	got, err := d.Read(key)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new-value" {
		t.Fatalf("read %q: got %q, want %q", key, got, "new-value")
	}
}

func TestMigrateAllowsConcurrentReadsAndWrites(t *testing.T) {
	oldBasePath := filepath.Join(t.TempDir(), "old")
	newBasePath := filepath.Join(t.TempDir(), "new")

	d := New(Options{
		BasePath:     oldBasePath,
		Transform:    func(string) []string { return nil },
		CacheSizeMax: 1024,
	})
	defer d.EraseAll()

	const keyCount = 100
	for i := 0; i < keyCount; i++ {
		key := fmt.Sprintf("key-%03d", i)
		if err := d.Write(key, []byte("initial-"+key)); err != nil {
			t.Fatal(err)
		}
	}

	started := make(chan struct{})
	d.migrationHook = func() {
		close(started)
	}

	migrated := make(chan error, 1)
	go func() {
		migrated <- d.Migrate(newBasePath)
	}()
	<-started

	var wg sync.WaitGroup
	errs := make(chan error, keyCount)
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				key := fmt.Sprintf("key-%03d", (worker*17+i)%keyCount)
				value := fmt.Sprintf("worker-%d-value-%d", worker, i)
				if err := d.Write(key, []byte(value)); err != nil {
					errs <- err
					return
				}
				if _, err := d.Read(key); err != nil {
					errs <- err
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	if err := <-migrated; err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for i := 0; i < keyCount; i++ {
		key := fmt.Sprintf("key-%03d", i)
		value := "final-" + key
		if err := d.Write(key, []byte(value)); err != nil {
			t.Fatal(err)
		}
		got, err := d.Read(key)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != value {
			t.Fatalf("read %q: got %q, want %q", key, got, value)
		}
	}
}
