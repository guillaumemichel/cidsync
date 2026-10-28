package main

import (
	"crypto/sha256"
	"encoding/base32"
	"io"
	"os"
)

// Codecs and hash functions, as multicodec numbers.
const (
	codecRaw     = 0x55 // a file's bytes
	codecDagCBOR = 0x71 // a CBOR block: a veritiles descriptor
	codecDagPB   = 0x70 // a UnixFS node, which a descriptor may name
	hashSHA256   = 0x12
	hashBlake3   = 0x1e
)

var base32Lower = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// cidText writes a CIDv1 with a 32-byte digest the way the index and
// veritiles read it: "b", then the bytes in lowercase base32.
func cidText(codec, hash byte, digest []byte) string {
	return "b" + base32Lower.EncodeToString(append([]byte{1, codec, hash, 32}, digest...))
}

func sumCID(codec byte, data []byte) string {
	sum := sha256.Sum256(data)
	return cidText(codec, hashSHA256, sum[:])
}

// fileCID is a file's raw sha2-256 CID, the one veritiles reads a raw asset
// by, and the bytes it read.
func fileCID(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return cidText(codecRaw, hashSHA256, h.Sum(nil)), n, nil
}
