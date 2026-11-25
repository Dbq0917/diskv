package diskv

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func waitForExpired(t *testing.T, d *Diskv, key string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, err := d.Read(key)
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			t.Fatalf("read %q while waiting for expiry: %v", key, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("key %q did not expire before deadline", key)
}

func waitForFileGone(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %q still exists", path)
}

func TestExpirationReadBatch(t *testing.T) {
	d := New(Options{
		BasePath:     "test-expiry-batch",
		CacheSizeMax: 1024,
	})
	defer d.EraseAll()

	now := time.Now()
	cases := []struct {
		key       string
		value     string
		expiresAt time.Time
	}{
		{"first", "one", now.Add(200 * time.Millisecond)},
		{"second", "two", now.Add(700 * time.Millisecond)},
		{"third", "three", now.Add(1200 * time.Millisecond)},
		{"forever", "four", time.Time{}},
	}
	for _, tc := range cases {
		if err := d.WriteWithExpiry(tc.key, []byte(tc.value), tc.expiresAt); err != nil {
			t.Fatalf("write %q: %v", tc.key, err)
		}
	}

	for _, tc := range cases {
		value, err := d.Read(tc.key)
		if err != nil {
			t.Fatalf("read %q before expiry: %v", tc.key, err)
		}
		if string(value) != tc.value {
			t.Fatalf("read %q: want %q, got %q", tc.key, tc.value, value)
		}
		if !d.Has(tc.key) {
			t.Fatalf("Has(%q) = false before expiry", tc.key)
		}
	}

	waitForExpired(t, d, "first")
	if _, err := d.Read("first"); !os.IsNotExist(err) {
		t.Fatalf("expired key error: want not-exist, got %v", err)
	}
	if d.Has("first") {
		t.Fatalf("expired key still reported present")
	}
	if got := d.ReadString("first"); got != "" {
		t.Fatalf("expired ReadString = %q, want empty", got)
	}
	for _, key := range []string{"second", "third", "forever"} {
		if _, err := d.Read(key); err != nil {
			t.Fatalf("read %q after another key expired: %v", key, err)
		}
	}
	assertKeysExclude(t, d, "first")

	waitForExpired(t, d, "second")
	for _, key := range []string{"third", "forever"} {
		if _, err := d.Read(key); err != nil {
			t.Fatalf("read %q after second expiry: %v", key, err)
		}
	}

	waitForExpired(t, d, "third")
	if value, err := d.Read("forever"); err != nil || string(value) != "four" {
		t.Fatalf("non-expiring key after all expiries: value=%q err=%v", value, err)
	}
}

func assertKeysExclude(t *testing.T, d *Diskv, excluded string) {
	t.Helper()
	for key := range d.Keys(nil) {
		if key == excluded {
			t.Fatalf("Keys yielded expired key %q", excluded)
		}
	}
}

func TestExpirationSurvivesRestart(t *testing.T) {
	basePath := "test-expiry-restart"
	d1 := New(Options{BasePath: basePath})
	defer d1.EraseAll()

	key := "session"
	expiresAt := time.Now().Add(350 * time.Millisecond)
	if err := d1.WriteStringWithExpiry(key, "token", expiresAt); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(d1.completeFilename(d1.transform(key)))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSticky == 0 {
		t.Fatalf("expiring file is missing its expiration marker")
	}
	if delta := fi.ModTime().Sub(expiresAt); delta < -time.Millisecond || delta > time.Millisecond {
		t.Fatalf("stored expiry %v differs from requested %v", fi.ModTime(), expiresAt)
	}

	d2 := New(Options{BasePath: basePath})
	if value, err := d2.Read(key); err != nil || string(value) != "token" {
		t.Fatalf("read after restart before expiry: value=%q err=%v", value, err)
	}

	waitForExpired(t, d2, key)
	if !time.Now().After(expiresAt) {
		t.Fatalf("key expired before its stored instant")
	}

	d3 := New(Options{BasePath: basePath})
	if _, err := d3.Read(key); !os.IsNotExist(err) {
		t.Fatalf("read after restart and expiry: want not-exist, got %v", err)
	}
}

func TestExpirationWriteForms(t *testing.T) {
	d := New(Options{
		BasePath:     "test-expiry-forms",
		TempDir:      "test-expiry-forms-temp",
		CacheSizeMax: 1024,
		Compression:  NewGzipCompression(),
	})
	defer d.EraseAll()

	expiresAt := time.Now().Add(time.Hour)
	if err := d.Write("bytes", []byte("bytes-value"), expiresAt); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteString("string", "string-value", expiresAt); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteStream("stream", bytes.NewBufferString("stream-value"), false, expiresAt); err != nil {
		t.Fatal(err)
	}

	for key, want := range map[string]string{
		"bytes":  "bytes-value",
		"string": "string-value",
		"stream": "stream-value",
	} {
		got, err := d.Read(key)
		if err != nil || string(got) != want {
			t.Fatalf("read %q: value=%q err=%v", key, got, err)
		}
		gotExpiry, ok, err := d.ExpiresAt(key)
		if err != nil || !ok {
			t.Fatalf("ExpiresAt(%q): ok=%v err=%v", key, ok, err)
		}
		if delta := gotExpiry.Sub(expiresAt); delta < -time.Millisecond || delta > time.Millisecond {
			t.Fatalf("ExpiresAt(%q) = %v, want %v", key, gotExpiry, expiresAt)
		}
	}
}

func TestExpiringKeysAreNotServedFromCache(t *testing.T) {
	d := New(Options{
		BasePath:     "test-expiry-cache",
		CacheSizeMax: 1024,
	})
	defer d.EraseAll()

	key := "short"
	if err := d.WriteWithTTL(key, []byte("value"), 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if value, err := d.Read(key); err != nil || string(value) != "value" {
		t.Fatalf("read before expiry: value=%q err=%v", value, err)
	}
	if d.isCached(key) {
		t.Fatalf("expiring value was cached and could outlive its file")
	}

	waitForExpired(t, d, key)
}

func TestCleanExpiredRemovesOnlyExpiredKeys(t *testing.T) {
	d := New(Options{BasePath: "test-expiry-clean"})
	defer d.EraseAll()

	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	keys := []struct {
		key       string
		expiresAt time.Time
	}{
		{"expired", past},
		{"live", future},
		{"forever", time.Time{}},
	}
	for _, item := range keys {
		if err := d.WriteWithExpiry(item.key, []byte(item.key), item.expiresAt); err != nil {
			t.Fatal(err)
		}
	}

	if err := d.CleanExpired(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(d.completeFilename(d.transform("expired"))); !os.IsNotExist(err) {
		t.Fatalf("expired file still exists: %v", err)
	}
	for _, key := range []string{"live", "forever"} {
		if _, err := os.Stat(d.completeFilename(d.transform(key))); err != nil {
			t.Fatalf("unexpired file %q was removed: %v", key, err)
		}
		if value, err := d.Read(key); err != nil || string(value) != key {
			t.Fatalf("read %q after cleanup: value=%q err=%v", key, value, err)
		}
	}

	var files []string
	err := filepath.Walk(d.BasePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			rel, _ := filepath.Rel(d.BasePath, path)
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	want := []string{"forever", "live"}
	if fmt.Sprint(files) != fmt.Sprint(want) {
		t.Fatalf("directory contents: want %v, got %v", want, files)
	}
}

func TestLegacyFileWithoutExpirationMarkerDoesNotExpire(t *testing.T) {
	d := New(Options{BasePath: "test-expiry-legacy"})
	defer d.EraseAll()

	key := "legacy"
	pathKey := d.transform(key)
	if err := os.MkdirAll(d.pathFor(pathKey), d.PathPerm); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(d.completeFilename(pathKey), []byte("old"), 0666); err != nil {
		t.Fatal(err)
	}

	value, err := d.Read(key)
	if err != nil {
		t.Fatalf("read legacy file: %v", err)
	}
	if !bytes.Equal(value, []byte("old")) {
		t.Fatalf("legacy value: want old, got %q", value)
	}
}

func TestCleanExpiredDoesNotBlockReadsOrWrites(t *testing.T) {
	d := New(Options{
		BasePath:     "test-expiry-concurrent",
		CacheSizeMax: 1024,
	})
	defer d.EraseAll()

	past := time.Now().Add(-time.Hour)
	for i := 0; i < 300; i++ {
		key := fmt.Sprintf("expired-%03d", i)
		if err := d.WriteWithExpiry(key, []byte("dead"), past); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan error, 1)
	go func() { done <- d.CleanExpired() }()

	deadline := time.Now().Add(5 * time.Second)
	for i := 0; i < 100; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("reads and writes were blocked by cleanup")
		}
		if err := d.WriteString("live", "value"); err != nil {
			t.Fatalf("write during cleanup: %v", err)
		}
		if value, err := d.Read("live"); err != nil || string(value) != "value" {
			t.Fatalf("read during cleanup: value=%q err=%v", value, err)
		}
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not finish")
	}
}

func TestBackgroundExpirationCleanup(t *testing.T) {
	d := New(Options{
		BasePath:        "test-expiry-background",
		CleanupInterval: 20 * time.Millisecond,
	})
	defer d.Close()
	defer d.EraseAll()

	key := "short"
	if err := d.WriteWithTTL(key, []byte("value"), 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	waitForFileGone(t, d.completeFilename(d.transform(key)))
}
