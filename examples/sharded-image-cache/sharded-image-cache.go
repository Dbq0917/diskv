package main

// This example models an image cache with hash-like keys. A flat directory
// turns into a linear scan once it holds hundreds of thousands of files: a
// full directory listing and every prefix lookup both walk every entry, and
// daily cleanup has to unlink files one by one from the same giant directory.
//
// PrefixLayout replaces the flat directory with a fixed-depth prefix tree.
// With 2-character hex shards and depth 1, one million keys spread across at
// most 256 leaf directories (~3900 files each). The number of directories is
// bounded regardless of how many keys are stored, so inode usage stays flat,
// and a prefix lookup descends straight to the relevant subtree instead of
// scanning the whole store.

import (
	"crypto/md5"
	"fmt"
	"io"

	"github.com/peterbourgon/diskv/v3"
)

func main() {
	layout, err := diskv.NewPrefixLayout("0123456789abcdef", 2, 1, "_")
	if err != nil {
		panic(err)
	}

	d := diskv.New(diskv.Options{
		BasePath:          "image-cache",
		AdvancedTransform: layout.AdvancedTransform,
		InverseTransform:  layout.InverseTransform,
		PrefixLocator:     layout,
		CacheSizeMax:      1024 * 1024, // 1MB
	})

	for _, imageURL := range []string{
		"https://example.com/a.png",
		"https://example.com/b.png",
		"https://example.com/c.png",
	} {
		key := md5sum(imageURL)
		if err := d.WriteString(key, "fake-image-bytes"); err != nil {
			panic(err)
		}

		// Stored at image-cache/<key[0:2]>/<key>, for example
		// image-cache/e2/e2fc0e... — at most 256 leaf directories.
		fmt.Printf("wrote %s\n", key)
	}

	// Prefix lookup only walks the matching subtree, e.g. image-cache/e2.
	count := 0
	for key := range d.KeysPrefix("e2fc", nil) {
		fmt.Printf("prefix match: %s\n", key)
		count++
	}
	fmt.Printf("%d key(s) matched prefix\n", count)
}

func md5sum(s string) string {
	h := md5.New()
	io.WriteString(h, s)
	return fmt.Sprintf("%x", h.Sum(nil))
}
