package diskv

import (
	"os"
	"reflect"
	"testing"
)

func testLayout(t *testing.T) *PrefixLayout {
	t.Helper()
	l, err := NewPrefixLayout("0123456789abcdef", 2, 2, "_")
	if err != nil {
		t.Fatalf("NewPrefixLayout: %s", err)
	}
	return l
}

func TestPrefixLayoutRejectsBadArguments(t *testing.T) {
	for _, tc := range []struct {
		name         string
		alphabet     string
		width, depth int
		pad          string
	}{
		{"zero width", "ab", 0, 2, "_"},
		{"zero depth", "ab", 2, 0, "_"},
		{"empty alphabet", "", 2, 2, "_"},
		{"empty pad", "ab", 2, 2, ""},
		{"pad with separator", "ab", 2, 2, "a/b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewPrefixLayout(tc.alphabet, tc.width, tc.depth, tc.pad); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestPrefixLayoutTransform(t *testing.T) {
	l := testLayout(t)

	for _, tc := range []struct {
		key  string
		want []string
	}{
		{"abcdef0123", []string{"ab", "cd"}},
		{"abc", []string{"ab", "c"}},
		{"a", []string{"a", "_"}},
		{"", []string{"_", "_"}},
	} {
		if have := l.Transform(tc.key); !reflect.DeepEqual(tc.want, have) {
			t.Errorf("Transform(%q): want %v, have %v", tc.key, tc.want, have)
		}
	}
}

func TestPrefixLayoutRoots(t *testing.T) {
	l := testLayout(t)

	for _, tc := range []struct {
		prefix string
		want   [][]string
	}{
		{"", [][]string{{}}},
		{"a", [][]string{{}}},
		{"ab", [][]string{{"ab"}}},
		{"abc", [][]string{{"ab"}}},
		{"abcd", [][]string{{"ab", "cd"}}},
		{"abcde", [][]string{{"ab", "cd"}}},
	} {
		if have := l.PrefixRoots(tc.prefix); !reflect.DeepEqual(tc.want, have) {
			t.Errorf("PrefixRoots(%q): want %v, have %v", tc.prefix, tc.want, have)
		}
	}
}

func TestPrefixLayoutStoreRoundTrip(t *testing.T) {
	l := testLayout(t)
	d := New(Options{
		BasePath:          "test-prefix-layout",
		AdvancedTransform: l.AdvancedTransform,
		InverseTransform:  l.InverseTransform,
		PrefixLocator:     l,
	})
	defer d.EraseAll()

	keys := []string{
		"abcdef0123",
		"abcdef9999",
		"abab000000",
		"a",
		"ab",
		"abc",
		"0000000000",
		"ffeeddccbb",
	}
	for _, key := range keys {
		if err := d.Write(key, []byte("value-"+key)); err != nil {
			t.Fatalf("Write(%q): %s", key, err)
		}
	}

	for _, key := range keys {
		got, err := d.Read(key)
		if err != nil {
			t.Fatalf("Read(%q): %s", key, err)
		}
		if want := "value-" + key; string(got) != want {
			t.Errorf("Read(%q): want %q, have %q", key, want, string(got))
		}
	}

	if err := d.Erase("abcdef0123"); err != nil {
		t.Fatalf("Erase: %s", err)
	}
	if d.Has("abcdef0123") {
		t.Error("erased key still present")
	}
}

func TestPrefixLayoutKeysPrefix(t *testing.T) {
	l := testLayout(t)
	d := New(Options{
		BasePath:          "test-prefix-layout-prefix",
		AdvancedTransform: l.AdvancedTransform,
		InverseTransform:  l.InverseTransform,
		PrefixLocator:     l,
	})
	defer d.EraseAll()

	keys := map[string]struct{}{}
	for _, key := range []string{
		"abcdef0123",
		"abcdef9999",
		"abab000000",
		"a",
		"ab",
		"abc",
		"0000000000",
		"ffeeddccbb",
	} {
		keys[key] = struct{}{}
		if err := d.Write(key, []byte(key)); err != nil {
			t.Fatalf("Write(%q): %s", key, err)
		}
	}

	for _, prefix := range []string{
		"",
		"a",
		"ab",
		"abc",
		"abcd",
		"abcdef",
		"abcdef0123",
		"abcdef0123zz",
		"0",
		"ff",
		"ffe",
		"x",
	} {
		have := map[string]struct{}{}
		for key := range d.KeysPrefix(prefix, nil) {
			have[key] = struct{}{}
		}

		want := map[string]struct{}{}
		for key := range keys {
			if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
				want[key] = struct{}{}
			}
		}

		if !reflect.DeepEqual(want, have) {
			t.Errorf("prefix %q: want %v, have %v", prefix, want, have)
		}
	}
}

func TestPrefixLayoutMissingPrefixRoots(t *testing.T) {
	l := testLayout(t)
	d := New(Options{
		BasePath:          "test-prefix-layout-missing",
		AdvancedTransform: l.AdvancedTransform,
		InverseTransform:  l.InverseTransform,
		PrefixLocator:     l,
	})
	defer d.EraseAll()

	if err := d.Write("abcdef0123", []byte("x")); err != nil {
		t.Fatalf("Write: %s", err)
	}

	for key := range d.KeysPrefix("zz", nil) {
		t.Fatalf("unexpected key %q for missing prefix", key)
	}
}

func TestPrefixLayoutErasePrunesDirs(t *testing.T) {
	l, err := NewPrefixLayout("0123456789abcdef", 2, 2, "_")
	if err != nil {
		t.Fatal(err)
	}
	d := New(Options{
		BasePath:          "test-prefix-layout-prune",
		AdvancedTransform: l.AdvancedTransform,
		InverseTransform:  l.InverseTransform,
		PrefixLocator:     l,
	})
	defer d.EraseAll()

	keys := []string{"ab12cd34", "ab12ef56", "ab34cd78"}
	for _, key := range keys {
		if err := d.Write(key, []byte(key)); err != nil {
			t.Fatalf("Write(%q): %s", key, err)
		}
	}

	for _, key := range keys {
		if err := d.Erase(key); err != nil {
			t.Fatalf("Erase(%q): %s", key, err)
		}
	}

	entries, err := os.ReadDir("test-prefix-layout-prune")
	if err != nil {
		t.Fatalf("base path should be retained after the last Erase: %s", err)
	}
	if len(entries) != 0 {
		t.Fatalf("sharding dirs not pruned, remaining entries: %v", entries)
	}

	// The store must be reusable after pruning removed its directories.
	if err := d.Write(keys[0], []byte("again")); err != nil {
		t.Fatalf("rewrite after pruning: %s", err)
	}
	if got, err := d.Read(keys[0]); err != nil || string(got) != "again" {
		t.Fatalf("read after rewrite = %q, %v", string(got), err)
	}
}

const benchKeyCount = 10000

func benchHexKeys() []string {
	keys := make([]string, benchKeyCount)
	for i := range keys {
		keys[i] = benchHexKey(i)
	}
	return keys
}

func benchHexKey(i int) string {
	const hex = "0123456789abcdef"
	b := make([]byte, 32)
	for j := range b {
		b[j] = hex[(i>>(uint(j)%28))&0xf]
	}
	return string(b)
}

func benchLayout() *PrefixLayout {
	l, _ := NewPrefixLayout("0123456789abcdef", 2, 1, "_")
	return l
}

func benchLoad(b *testing.B, d *Diskv, keys []string) {
	b.Helper()
	value := make([]byte, 1024)
	for _, key := range keys {
		if err := d.Write(key, value); err != nil {
			b.Fatalf("Write: %s", err)
		}
	}
}

func BenchmarkKeysPrefix_All_Flat(b *testing.B) {
	d := New(Options{BasePath: "bench-flat"})
	defer d.EraseAll()
	benchLoad(b, d, benchHexKeys())

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		count := 0
		for range d.Keys(nil) {
			count++
		}
		if count != benchKeyCount {
			b.Fatalf("count = %d, want %d", count, benchKeyCount)
		}
	}
}

func BenchmarkKeysPrefix_All_Sharded(b *testing.B) {
	l := benchLayout()
	d := New(Options{
		BasePath:          "bench-sharded",
		AdvancedTransform: l.AdvancedTransform,
		InverseTransform:  l.InverseTransform,
		PrefixLocator:     l,
	})
	defer d.EraseAll()
	benchLoad(b, d, benchHexKeys())

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		count := 0
		for range d.Keys(nil) {
			count++
		}
		if count != benchKeyCount {
			b.Fatalf("count = %d, want %d", count, benchKeyCount)
		}
	}
}

func BenchmarkKeysPrefix_OneShard_Flat(b *testing.B) {
	d := New(Options{BasePath: "bench-flat-prefix"})
	defer d.EraseAll()
	benchLoad(b, d, benchHexKeys())

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for range d.KeysPrefix("ab", nil) {
		}
	}
}

func BenchmarkKeysPrefix_OneShard_Sharded(b *testing.B) {
	l := benchLayout()
	d := New(Options{
		BasePath:          "bench-sharded-prefix",
		AdvancedTransform: l.AdvancedTransform,
		InverseTransform:  l.InverseTransform,
		PrefixLocator:     l,
	})
	defer d.EraseAll()
	benchLoad(b, d, benchHexKeys())

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for range d.KeysPrefix("ab", nil) {
		}
	}
}
