package diskv

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// countDirs returns the number of directories under root, excluding root.
func countDirs(t *testing.T, root string) int {
	t.Helper()
	count := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && path != root {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("countDirs: %s", err)
	}
	return count
}

// maxDepth returns the maximum directory depth below root (root itself is 0).
func maxDepth(t *testing.T, root string) int {
	t.Helper()
	depth := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if d := len(strings.Split(filepath.ToSlash(path), "/")) - len(strings.Split(filepath.ToSlash(root), "/")); d > depth {
				depth = d
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("maxDepth: %s", err)
	}
	return depth
}

// Keys containing slashes (hierarchical business IDs) must not create one
// directory per key level. The default transform shards files into a
// bounded two-level structure, and erasing keys prunes directories again.
func TestSlashKeysFlattened(t *testing.T) {
	const basePath = "test-data-flattened"
	d := New(Options{
		BasePath:     basePath,
		CacheSizeMax: 1024,
	})
	defer d.EraseAll()

	const keyCount = 2000
	keys := make([]string, 0, keyCount)
	for i := 0; i < keyCount; i++ {
		keys = append(keys, fmt.Sprintf("tenant-%03d/region-%d/%08d", i%7, i%3, i))
	}

	for _, k := range keys {
		if err := d.WriteString(k, "value-"+k); err != nil {
			t.Fatalf("write %q: %s", k, err)
		}
	}

	for _, k := range keys {
		if want, have := "value-"+k, d.ReadString(k); want != have {
			t.Fatalf("read %q: want %q, have %q", k, want, have)
		}
	}

	// The directory hierarchy must converge: bounded depth, and a directory
	// count that does not scale with the number of keys.
	const maxDirs = 256 + 256*256 // one byte per level, two levels
	if n := countDirs(t, basePath); n > maxDirs {
		t.Fatalf("directory count %d exceeds bounded maximum %d", n, maxDirs)
	}
	if depth := maxDepth(t, basePath); depth > 2 {
		t.Fatalf("directory depth %d exceeds bounded maximum 2", depth)
	}

	// Keys must round-trip through the store unmodified.
	seen := map[string]bool{}
	for k := range d.Keys(nil) {
		seen[k] = true
	}
	for _, k := range keys {
		if !seen[k] {
			t.Errorf("Keys() did not yield %q", k)
		}
	}

	// Prefix scans over slash-containing keys must still work.
	prefix := "tenant-001/"
	for k := range d.KeysPrefix(prefix, nil) {
		if !strings.HasPrefix(k, prefix) {
			t.Errorf("KeysPrefix(%q) yielded %q", prefix, k)
		}
		delete(seen, k)
	}

	// The key-to-path mapping must be stable across store instances.
	d2 := New(Options{BasePath: basePath})
	for _, k := range keys {
		if !d2.Has(k) {
			t.Fatalf("reopened store is missing %q", k)
		}
	}

	// Erasing keys must remove their directories, not just the files.
	for _, k := range keys {
		if err := d.Erase(k); err != nil {
			t.Fatalf("erase %q: %s", k, err)
		}
	}
	if n := countDirs(t, basePath); n != 0 {
		t.Fatalf("%d directories left after erasing all keys", n)
	}
}

func TestEscapeKeyRoundTrip(t *testing.T) {
	for _, key := range []string{
		"a",
		"tenant/region/id",
		"/leading",
		"trailing/",
		"consecutive//slashes",
		"100%",
		"%2F",
		"a%2Fb",
		"%zz",
		"mixed%/and/more%",
		".hidden/file",
	} {
		escaped := escapeKey(key)
		if strings.ContainsRune(escaped, '/') {
			t.Errorf("escapeKey(%q) = %q still contains a slash", key, escaped)
		}
		if have := unescapeKey(escaped); have != key {
			t.Errorf("round trip failed: escapeKey(%q) = %q, unescapeKey = %q", key, escaped, have)
		}
	}
}

func TestDefaultTransformStable(t *testing.T) {
	key := "tenant-001/region-2/00000042"
	first := defaultAdvancedTransform(key)
	second := defaultAdvancedTransform(key)

	if len(first.Path) != 2 {
		t.Fatalf("default transform path depth = %d, want 2", len(first.Path))
	}
	for i := range first.Path {
		if first.Path[i] != second.Path[i] {
			t.Fatalf("unstable mapping: %v vs %v", first.Path, second.Path)
		}
	}
	if first.FileName != second.FileName {
		t.Fatalf("unstable file name: %q vs %q", first.FileName, second.FileName)
	}
	if strings.ContainsRune(first.FileName, '/') {
		t.Fatalf("file name %q contains a slash", first.FileName)
	}
	if have := defaultInverseTransform(first); have != key {
		t.Fatalf("inverse transform = %q, want %q", have, key)
	}
}
