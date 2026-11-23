package diskv

import (
	"errors"
	"path/filepath"
	"strings"
)

// PrefixLocator is implemented by transforms whose sharding is derived
// deterministically from leading characters of the key. It allows KeysPrefix
// to enumerate only the directory subtrees that can contain keys with the
// given prefix, instead of walking the entire store.
//
// PrefixRoots returns, relative to the store BasePath, the roots of all
// directory subtrees that may hold keys matching prefix. An empty result means
// no key can possibly match the prefix.
type PrefixLocator interface {
	PrefixRoots(prefix string) [][]string
}

// PrefixLayout is a fixed-depth, prefix-based sharding layout.
//
// Every key is split into depth shards of width characters taken from the
// beginning of the key; the shards become the directory path, and the full
// key remains the file name. Keys shorter than width*depth characters are
// right-padded with pad shards so that the directory depth stays constant.
//
// Because the depth is fixed, the number of directories has a hard upper
// bound independent of the number of keys, and data files only ever exist at
// the leaf level, so a key can never collide with a directory. The leading
// shards of a key are also the leading characters of the key itself, which
// makes prefix lookups locatable on disk.
type PrefixLayout struct {
	alphabet string
	width    int
	depth    int
	pad      string
}

// NewPrefixLayout returns a PrefixLayout that shards keys into depth directory
// levels, each made of width leading characters of the key. alphabet defines
// the characters that may appear in a shard and pad is the shard used to fill
// the path of keys that are shorter than width*depth characters.
//
// For hexadecimal keys, a typical choice for around one million keys is
// NewPrefixLayout("0123456789abcdef", 2, 2, "_"), yielding at most 256 leaf
// directories.
func NewPrefixLayout(alphabet string, width, depth int, pad string) (*PrefixLayout, error) {
	switch {
	case width <= 0:
		return nil, errors.New("prefix layout: width must be positive")
	case depth <= 0:
		return nil, errors.New("prefix layout: depth must be positive")
	case len(alphabet) == 0:
		return nil, errors.New("prefix layout: alphabet must not be empty")
	case pad == "":
		return nil, errors.New("prefix layout: pad must not be empty")
	case strings.ContainsAny(pad, string(filepath.Separator)):
		return nil, errors.New("prefix layout: pad must not contain a path separator")
	case strings.ContainsAny(alphabet, string(filepath.Separator)):
		return nil, errors.New("prefix layout: alphabet must not contain a path separator")
	}
	return &PrefixLayout{alphabet: alphabet, width: width, depth: depth, pad: pad}, nil
}

// shards splits key into exactly depth shards. Shards that would extend past
// the end of the key are filled with the configured pad shard, and a final
// shard that only partially fits keeps the remaining characters as-is.
func (l *PrefixLayout) shards(key string) []string {
	shards := make([]string, l.depth)
	for i := range shards {
		from, to := i*l.width, (i+1)*l.width
		switch {
		case from >= len(key):
			shards[i] = l.pad
		case to > len(key):
			shards[i] = key[from:]
		default:
			shards[i] = key[from:to]
		}
	}
	return shards
}

// Transform implements TransformFunction.
func (l *PrefixLayout) Transform(key string) []string {
	return l.shards(key)
}

// AdvancedTransform implements AdvancedTransformFunction: the full key is kept
// as the file name so that InverseTransform can always recover it verbatim.
func (l *PrefixLayout) AdvancedTransform(key string) *PathKey {
	return &PathKey{Path: l.shards(key), FileName: key}
}

// InverseTransform implements InverseTransformFunction.
func (l *PrefixLayout) InverseTransform(pathKey *PathKey) string {
	return pathKey.FileName
}

// PrefixRoots implements PrefixLocator.
//
// Only fully-determined shards are used as exact directory components. The
// first shard the prefix does not fully determine stops the descent: every
// sibling shard sharing that fragment is a directory under the current root,
// so the walker enumerates the root subtree and filters leaf files against the
// full prefix. A prefix that ends exactly on a shard boundary keeps the full
// shard as a component, since it identifies a concrete directory.
func (l *PrefixLayout) PrefixRoots(prefix string) [][]string {
	if prefix == "" {
		return [][]string{{}}
	}

	roots := [][]string{}
	root := make([]string, 0, l.depth)
	for i := 0; i < l.depth; i++ {
		from, to := i*l.width, (i+1)*l.width
		switch {
		case from >= len(prefix):
			// The prefix is fully consumed: remaining shards (including pad
			// shards of short keys) are unconstrained.
			roots = append(roots, append([]string{}, root...))
			return roots
		case to == len(prefix):
			// Prefix ends exactly on a shard boundary: the shard identifies
			// one concrete directory.
			root = append(root, prefix[from:to])
			roots = append(roots, append([]string{}, root...))
			return roots
		case to < len(prefix):
			// Fully-determined interior shard.
			root = append(root, prefix[from:to])
		default:
			// Partial shard: no single directory is identified, so stop at
			// the parent root and let the walker filter the subtree.
			roots = append(roots, append([]string{}, root...))
			return roots
		}
	}
	roots = append(roots, append([]string{}, root...))
	return roots
}
