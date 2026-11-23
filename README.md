# What is diskv?

Diskv (disk-vee) is a simple, persistent key-value store written in the Go
language. It starts with an incredibly simple API for storing arbitrary data on
a filesystem by key, and builds several layers of performance-enhancing
abstraction on top.  The end result is a conceptually simple, but highly
performant, disk-backed storage system.

[![Build Status][1]][2]

[1]: https://drone.io/github.com/peterbourgon/diskv/status.png
[2]: https://drone.io/github.com/peterbourgon/diskv/latest


# Installing

Install [Go 1][3], either [from source][4] or [with a prepackaged binary][5].
Then,

```bash
$ go get github.com/peterbourgon/diskv/v3
```

[3]: http://golang.org
[4]: http://golang.org/doc/install/source
[5]: http://golang.org/doc/install


# Usage

```go
package main

import (
	"fmt"
	"github.com/peterbourgon/diskv/v3"
)

func main() {
	// Simplest transform function: put all the data files into the base dir.
	flatTransform := func(s string) []string { return []string{} }

	// Initialize a new diskv store, rooted at "my-data-dir", with a 1MB cache.
	d := diskv.New(diskv.Options{
		BasePath:     "my-data-dir",
		Transform:    flatTransform,
		CacheSizeMax: 1024 * 1024,
	})

	// Write three bytes to the key "alpha".
	key := "alpha"
	d.Write(key, []byte{'1', '2', '3'})

	// Read the value back out of the store.
	value, _ := d.Read(key)
	fmt.Printf("%v\n", value)

	// Erase the key+value from the store (and the disk).
	d.Erase(key)
}
```

More complex examples can be found in the "examples" subdirectory.


# Theory

## Basic idea

At its core, diskv is a map of a key (`string`) to arbitrary data (`[]byte`).
The data is written to a single file on disk, with the same name as the key.
The key determines where that file will be stored, via a user-provided
`TransformFunc`, which takes a key and returns a slice (`[]string`)
corresponding to a path list where the key file will be stored. The simplest
TransformFunc,

```go
func SimpleTransform (key string) []string {
    return []string{}
}
```

will place all keys in the same, base directory. The design is inspired by
[Redis diskstore][6]; a TransformFunc which emulates the default diskstore
behavior is available in the content-addressable-storage example.

[6]: http://groups.google.com/group/redis-db/browse_thread/thread/d444bc786689bde9?pli=1

**Note** that your TransformFunc should ensure that one valid key doesn't
transform to a subset of another valid key. That is, it shouldn't be possible
to construct valid keys that resolve to directory names. As a concrete example,
if your TransformFunc splits on every 3 characters, then

```go
d.Write("abcabc", val) // OK: written to <base>/abc/abc/abcabc
d.Write("abc", val)    // Error: attempted write to <base>/abc/abc, but it's a directory
```

This will be addressed in an upcoming version of diskv.

Probably the most important design principle behind diskv is that your data is
always flatly available on the disk. diskv will never do anything that would
prevent you from accessing, copying, backing up, or otherwise interacting with
your data via common UNIX commandline tools.

## Advanced path transformation

If you need more control over the file name written to disk or if you want to support
slashes in your key name or special characters in the keys, you can use the
AdvancedTransform property. You must supply a function that returns
a special PathKey structure, which is a breakdown of a path and a file name. Strings
returned must be clean of any slashes or special characters:

```go
func AdvancedTransformExample(key string) *diskv.PathKey {
	path := strings.Split(key, "/")
	last := len(path) - 1
	return &diskv.PathKey{
		Path:     path[:last],
		FileName: path[last] + ".txt",
	}
}

// If you provide an AdvancedTransform, you must also provide its
// inverse:

func InverseTransformExample(pathKey *diskv.PathKey) (key string) {
	txt := pathKey.FileName[len(pathKey.FileName)-4:]
	if txt != ".txt" {
		panic("Invalid file found in storage folder!")
	}
	return strings.Join(pathKey.Path, "/") + pathKey.FileName[:len(pathKey.FileName)-4]
}

func main() {
	d := diskv.New(diskv.Options{
		BasePath:          "my-data-dir",
		AdvancedTransform: AdvancedTransformExample,
		InverseTransform:  InverseTransformExample,
		CacheSizeMax:      1024 * 1024,
	})
	// Write some text to the key "alpha/beta/gamma".
	key := "alpha/beta/gamma"
	d.WriteString(key, "¡Hola!") // will be stored in "<basedir>/alpha/beta/gamma.txt"
	fmt.Println(d.ReadString("alpha/beta/gamma"))
}
```


## Adding a cache

An in-memory caching layer is provided by combining the BasicStore
functionality with a simple map structure, and keeping it up-to-date as
appropriate. Since the map structure in Go is not threadsafe, it's combined
with a RWMutex to provide safe concurrent access.

## Adding order

diskv is a key-value store and therefore inherently unordered. An ordering
system can be injected into the store by passing something which satisfies the
diskv.Index interface. (A default implementation, using Google's
[btree][7] package, is provided.) Basically, diskv keeps an ordered (by a
user-provided Less function) index of the keys, which can be queried.

[7]: https://github.com/google/btree

## Adding compression

Something which implements the diskv.Compression interface may be passed
during store creation, so that all Writes and Reads are filtered through
a compression/decompression pipeline. Several default implementations,
using stdlib compression algorithms, are provided. Note that data is cached
compressed; the cost of decompression is borne with each Read.

## Streaming

diskv also now provides ReadStream and WriteStream methods, to allow very large
data to be handled efficiently.

## Sharding large stores

The default flat layout puts every data file in a single directory. On most
filesystems a directory with hundreds of thousands of entries is expensive to
scan: a full key listing, a prefix lookup, and bulk cleanup all degrade into
linear walks of the whole directory, and `readdir` latency keeps growing with
the entry count.

`PrefixLayout` provides a fixed-depth, prefix-based sharding layout designed
for large, hash-keyed stores such as image caches:

```go
// 2-character hex shards, one level deep, "_" pads keys shorter than 2
// chars. One million keys spread across at most 256 leaf directories
// (~3900 files each); the directory count is bounded, so inode usage stays
// flat.
layout, err := diskv.NewPrefixLayout("0123456789abcdef", 2, 1, "_")
if err != nil {
    panic(err)
}

d := diskv.New(diskv.Options{
    BasePath:          "image-cache",
    AdvancedTransform: layout.AdvancedTransform,
    InverseTransform:  layout.InverseTransform,
    PrefixLocator:     layout, // enables prefix-aware KeysPrefix
})

// Key e2fc0e... is stored at image-cache/e2/e2fc0e...
d.WriteString("e2fc0e1234abcd", "...")

// KeysPrefix only walks image-cache/e2 instead of the whole store.
for key := range d.KeysPrefix("e2fc", nil) {
    fmt.Println(key)
}
```

Properties of the layout:

* The directory depth is fixed and the shard count is bounded by
  `len(alphabet)^width` per level, independent of the number of keys. Data
  files only exist at the leaf level, so a key can never collide with a
  directory name (the failure mode described in the Basic idea section).
* Shards are taken from the leading characters of the key, so a key prefix
  maps directly to a directory subtree. With `PrefixLocator` set, `KeysPrefix`
  enumerates only the matching subtrees; without it, behavior is unchanged and
  the whole base path is walked.
* The full key remains the file name, so the data stays inspectable and
  recoverable with ordinary UNIX tools.

### Sizing the shards

Choose `width` and `depth` so the number of possible leaf directories
(`len(alphabet)^width` raised to `depth`) is comfortably smaller than the
expected number of keys. A leaf directory should hold hundreds to low
thousands of files, not a handful.

For one million hexadecimal keys, a single 2-character level gives 256 leaves
of ~3900 files. Adding a second 2-character level raises the leaf cap to
65536, which over-shards the same dataset: most leaves stay nearly empty, the
directory (inode) count explodes, and full-store listings slow down. The
million-key scale test reports both layouts side by side; the rule of thumb
is to add another level only when a leaf directory itself grows past roughly
10k entries.

A runnable version lives in
`examples/sharded-image-cache`, and a million-key flat-vs-sharded comparison
is available via the `DISKV_SCALE_KEYS` environment variable:

```bash
$ DISKV_SCALE_KEYS=1000000 go test -run TestScaleMillionKeys -v -timeout 2h
```


# Future plans

 * Needs plenty of robust testing: huge datasets, etc...
 * More thorough benchmarking
 * Your suggestions for use-cases I haven't thought of


# Credits and contributions

Original idea, design and implementation: [Peter Bourgon](https://github.com/peterbourgon)
Other collaborations: [Javier Peletier](https://github.com/jpeletier) ([Epic Labs](https://www.epiclabs.io))
