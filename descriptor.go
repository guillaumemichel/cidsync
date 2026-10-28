package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
)

// A packed file X has its proofs beside it, in X.proofs/, and its
// descriptor at X.proofs/root (veritiles SPEC.md §2): a strict DRISL block
// whose CID is the anchor a page holds, and which names X by its map CID, a
// raw BLAKE3 CID, and gives its size. The publisher never hashes X: the
// descriptor says what it is.

const (
	maxDescriptor = 256 << 10  // veritiles reads a descriptor whole, up to this
	metaRecord    = 1 + 8 + 32 // kind, u64le length, digest
	maxUint       = 1 << 53    // exclusive: veritiles reads integers as JavaScript numbers
)

// The descriptor's keys, in DRISL order; a CID link follows "map" and
// "unixfs": tag 42, a 37-byte string, the identity multibase prefix.
var (
	keyV       = []byte("\x61v")
	keyMap     = []byte("\x63map\xd8\x2a\x58\x25\x00")
	keyMeta    = []byte("\x64meta")
	keyUnixfs  = []byte("\x66unixfs\xd8\x2a\x58\x25\x00")
	keyMapSize = []byte("\x67mapSize")
)

// A packing is what X.proofs/root says of X, and its own CID and size.
type packing struct {
	anchor   string
	rootSize int64
	mapCID   string
	mapSize  int64
}

type descriptor struct {
	mapCID  string
	mapSize int64
}

func readPacking(rootPath string) (packing, error) {
	f, err := os.Open(rootPath)
	if err != nil {
		return packing{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxDescriptor+1))
	if err != nil {
		return packing{}, err
	}
	if len(b) > maxDescriptor {
		return packing{}, fmt.Errorf("%s: over 256 KiB, not a descriptor", rootPath)
	}
	d, err := decodeDescriptor(b)
	if err != nil {
		return packing{}, fmt.Errorf("%s: %w", rootPath, err)
	}
	return packing{anchor: sumCID(codecDagCBOR, b), rootSize: int64(len(b)), mapCID: d.mapCID, mapSize: d.mapSize}, nil
}

// decodeDescriptor reads a descriptor as strictly as veritiles does: one of
// its two templates, nothing after it, and a meta covering mapSize bytes.
// A descriptor veritiles would refuse is not worth publishing.
func decodeDescriptor(b []byte) (descriptor, error) {
	f, err := descriptorFields(b)
	if err != nil {
		return descriptor{}, err
	}
	if !bytes.HasPrefix(f.mapCID, []byte{1, codecRaw, hashBlake3, 32}) {
		return descriptor{}, errors.New("map: not a raw BLAKE3 CID")
	}
	if f.unixfs != nil && !validUnixfs(f.unixfs) {
		return descriptor{}, errors.New("unixfs: not a raw or dag-pb sha2-256 CID")
	}
	covered, err := metaCoverage(f.meta)
	if err != nil {
		return descriptor{}, err
	}
	if f.mapSize != covered {
		return descriptor{}, fmt.Errorf("mapSize %d, but meta covers %d bytes", f.mapSize, covered)
	}
	return descriptor{mapCID: cidText(codecRaw, hashBlake3, f.mapCID[4:]), mapSize: int64(f.mapSize)}, nil
}

type fields struct {
	mapCID, meta, unixfs []byte
	mapSize              uint64
}

// descriptorFields walks the template: 0xa4 without unixfs, 0xa5 with it.
func descriptorFields(b []byte) (fields, error) {
	if len(b) == 0 || b[0] != 0xa4 && b[0] != 0xa5 {
		return fields{}, errors.New("not the descriptor template")
	}
	r := &cborReader{b: b, off: 1}
	var f fields
	r.expect(keyV)
	if v := r.head(0, "v"); r.err == nil && v != 1 {
		return fields{}, fmt.Errorf("version %d, want 1", v)
	}
	r.expect(keyMap)
	f.mapCID = r.next(36)
	r.expect(keyMeta)
	f.meta = r.next(r.head(2, "meta"))
	if b[0] == 0xa5 {
		r.expect(keyUnixfs)
		f.unixfs = r.next(36)
	}
	r.expect(keyMapSize)
	f.mapSize = r.head(0, "mapSize")
	switch {
	case r.err != nil:
		return fields{}, r.err
	case r.off != len(b):
		return fields{}, errors.New("trailing bytes")
	case f.mapSize >= maxUint:
		return fields{}, errors.New("mapSize: 2^53 or more")
	}
	return f, nil
}

func validUnixfs(c []byte) bool {
	return c[0] == 1 && (c[1] == codecRaw || c[1] == codecDagPB) && c[2] == hashSHA256 && c[3] == 32
}

// metaCoverage is the bytes a meta's records cover, each a shard (kind 0)
// or a directory (kind 1) of at least one byte.
func metaCoverage(meta []byte) (uint64, error) {
	if len(meta) == 0 || len(meta)%metaRecord != 0 {
		return 0, errors.New("meta: not whole records")
	}
	var covered uint64
	for record := range slices.Chunk(meta, metaRecord) {
		length := binary.LittleEndian.Uint64(record[1:9])
		if record[0] > 1 || length == 0 {
			return 0, errors.New("meta: a record of an unknown kind or no bytes")
		}
		if length >= maxUint || covered+length >= maxUint {
			return 0, errors.New("meta: covers 2^53 bytes or more")
		}
		covered += length
	}
	return covered, nil
}

// A cborReader reads the few CBOR items a descriptor holds. The first
// error sticks, and every read after it does nothing.
type cborReader struct {
	b   []byte
	off int
	err error
}

func (r *cborReader) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

// next takes n bytes.
func (r *cborReader) next(n uint64) []byte {
	if r.err != nil {
		return nil
	}
	if n > uint64(len(r.b)-r.off) {
		r.fail(errors.New("truncated"))
		return nil
	}
	out := r.b[r.off : r.off+int(n)]
	r.off += int(n)
	return out
}

// expect takes bytes the template fixes.
func (r *cborReader) expect(want []byte) {
	if got := r.next(uint64(len(want))); r.err == nil && !bytes.Equal(got, want) {
		r.fail(errors.New("not the descriptor template"))
	}
}

// head reads the head of an item of type major, and returns its argument:
// the value of an integer, the length of a string. DRISL writes it in its
// shortest form.
func (r *cborReader) head(major byte, what string) uint64 {
	h := r.next(1)
	if r.err != nil {
		return 0
	}
	info := h[0] & 31
	if h[0]>>5 != major || info > 27 {
		r.fail(errors.New("not the descriptor template"))
		return 0
	}
	if info < 24 {
		return uint64(info)
	}
	var arg uint64
	for _, c := range r.next(1 << (info - 24)) {
		arg = arg<<8 | uint64(c)
	}
	if shortest := [...]uint64{24, 1 << 8, 1 << 16, 1 << 32}[info-24]; r.err == nil && arg < shortest {
		r.fail(fmt.Errorf("%s: not a shortest-form integer", what))
	}
	return arg
}
