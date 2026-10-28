package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The fixture is a packing of numbers.txt, which is
// `seq 1 1000000`: 6,888,896 bytes.
const (
	numbersRaw    = "bafkreieqim74xwpbmkl6nj6b3lfravrzi5brsr3w4uxxr27qurfybnvrj4"
	numbersAnchor = "bafyreiekcfm5puidi6wv4cfgupg4gfwkutszix4q4wlmwgwuzfhkm5t3s4"
	numbersMap    = "bafkr4iec6oorssluzmp2fneli6zfbgqk7zgse2o3heoj72wxtd3d6cthgu"
	numbersSize   = 6_888_896
	numbersRoot   = "testdata/numbers.txt.proofs/root"
	helloRaw      = "bafkreicysg23kiwv34eg2d7qweipxwosdo2py4ldv42nbauguluen5v6am"
)

// numbers is `seq 1 1000000`, made once: tests run under -race.
var numbers = sync.OnceValue(func() []byte {
	var b bytes.Buffer
	for i := 1; i <= 1_000_000; i++ {
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('\n')
	}
	return b.Bytes()
})

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFileCID(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{"hello", []byte("hello\n"), helloRaw},
		{"numbers", numbers(), numbersRaw},
		{"empty", nil, "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"},
	}
	for _, tc := range tests {
		path := filepath.Join(dir, tc.name)
		writeFile(t, path, tc.data)
		got, n, err := fileCID(path)
		if err != nil || got != tc.want || n != int64(len(tc.data)) {
			t.Errorf("%s: fileCID = %s, %d, %v, want %s, %d", tc.name, got, n, err, tc.want, len(tc.data))
		}
	}
}

func TestReadPacking(t *testing.T) {
	p, err := readPacking(numbersRoot)
	if err != nil {
		t.Fatal(err)
	}
	want := packing{anchor: numbersAnchor, rootSize: 110, mapCID: numbersMap, mapSize: numbersSize}
	if p != want {
		t.Errorf("readPacking = %+v, want %+v", p, want)
	}
}

func TestReadPackingTooLarge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "root")
	writeFile(t, path, make([]byte, 256<<10+1))
	if _, err := readPacking(path); err == nil || !strings.Contains(err.Error(), "256 KiB") {
		t.Errorf("readPacking of 256 KiB + 1 = %v, want an error naming the bound", err)
	}
}

// withUnixfs is the fixture as the 0xa5 template: a raw sha2-256 unixfs CID
// between meta and mapSize.
func withUnixfs(t *testing.T, root []byte) []byte {
	t.Helper()
	at := bytes.Index(root, []byte("\x67mapSize"))
	unixfs := append([]byte("\x66unixfs\xd8\x2a\x58\x25\x00\x01\x55\x12\x20"), make([]byte, 32)...)
	out := append([]byte{0xa5}, root[1:at]...)
	return append(append(out, unixfs...), root[at:]...)
}

// withMeta is the fixture with another meta, of fewer than 256 bytes.
func withMeta(t *testing.T, root, meta []byte) []byte {
	t.Helper()
	at := bytes.Index(root, []byte("\x64meta")) + 5
	rest := root[at+2+41:]
	out := append(bytes.Clone(root[:at]), 0x58, byte(len(meta)))
	return append(append(out, meta...), rest...)
}

func TestDecodeDescriptor(t *testing.T) {
	root, err := os.ReadFile(numbersRoot)
	if err != nil {
		t.Fatal(err)
	}
	want := descriptor{mapCID: numbersMap, mapSize: numbersSize}
	for name, b := range map[string][]byte{"0xa4": root, "0xa5": withUnixfs(t, root)} {
		if d, err := decodeDescriptor(b); err != nil || d != want {
			t.Errorf("%s: decodeDescriptor = %+v, %v, want %+v", name, d, err, want)
		}
	}
}

func TestDecodeDescriptorRejects(t *testing.T) {
	root, err := os.ReadFile(numbersRoot)
	if err != nil {
		t.Fatal(err)
	}
	// edit returns the fixture with the bytes at the first occurrence of
	// old replaced by new.
	edit := func(old, new string) []byte {
		at := bytes.Index(root, []byte(old))
		if at < 0 {
			t.Fatalf("%q not in the fixture", old)
		}
		out := bytes.Clone(root)
		copy(out[at:], new)
		return out
	}
	tests := []struct {
		name string
		b    []byte
		want string
	}{
		{"empty", nil, "template"},
		{"another map head", append([]byte{0xa3}, root[1:]...), "template"},
		{"version 2", edit("\x61v\x01", "\x61v\x02"), "version"},
		{"a key out of order", edit("\x63map", "\x63mab"), "template"},
		{"map a dag-cbor CID", edit("\x00\x01\x55\x1e", "\x00\x01\x71\x1e"), "map"},
		{"map a sha2-256 CID", edit("\x00\x01\x55\x1e", "\x00\x01\x55\x12"), "map"},
		{"meta not whole records", withMeta(t, root, root[bytes.Index(root, []byte("\x64meta"))+7:][:40]), "meta"},
		{"meta of an unknown kind", edit("\x64meta\x58\x29\x00", "\x64meta\x58\x29\x02"), "meta"},
		{"mapSize past the meta", edit("\x1a\x00\x69\x1d\xc0", "\x1a\x00\x69\x1d\xc1"), "mapSize"},
		{"mapSize not shortest", append(bytes.Clone(root[:len(root)-5]), 0x1b, 0, 0, 0, 0, 0, 0x69, 0x1d, 0xc0), "integer"},
		{"truncated", root[:len(root)-1], "truncated"},
		{"trailing bytes", append(bytes.Clone(root), 0), "trailing"},
	}
	for _, tc := range tests {
		d, err := decodeDescriptor(tc.b)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: decodeDescriptor = %+v, %v, want an error mentioning %q", tc.name, d, err, tc.want)
		}
	}
}
