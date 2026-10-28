package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

const (
	maxPaths    = 16  // the index keeps a CID's first 16 URLs
	maxURLBytes = 256 // the index's bound on a URL as sent
	proofsDir   = ".proofs"
)

// An entry is what a section offers of one CID: its paths below the
// section's base, escaped and sorted, and its size.
type entry struct {
	urls []string
	size int64
}

// A found file is one the walk keeps: its path below the directory and its
// stamp, and its descriptor's when it is packed.
type found struct {
	rel  string // slash-separated, as on disk
	abs  string
	at   stamp
	root *stamp
}

// A file is a published path and the CID of the bytes there.
type file struct {
	path string
	cid  string
	size int64
}

// scanSection reads a section's directory into what it offers, by CID.
func scanSection(sec *Section, cache *hashCache, log *slog.Logger) (map[string]entry, error) {
	found, err := walk(sec.Dir, sec.Exclude, log)
	if err != nil {
		return nil, err
	}
	files, err := identify(found, cache, log)
	if err != nil {
		return nil, err
	}
	return entries(files, log), nil
}

// walk lists the files below dir that a section publishes. A symlinked
// file is followed, a symlinked directory is not.
func walk(dir string, exclude []string, log *slog.Logger) ([]found, error) {
	var out []found
	err := filepath.WalkDir(dir, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return walkError(abs == dir, err)
		}
		rel, err := filepath.Rel(dir, abs)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case skipped(rel, exclude) && d.IsDir():
			return fs.SkipDir
		case skipped(rel, exclude):
			return nil
		case strings.HasSuffix(rel, proofsDir) && (d.IsDir() || d.Type()&fs.ModeSymlink != 0):
			return proofs(abs, rel, d, log)
		case d.IsDir():
			return nil
		}
		f, ok := foundFile(abs, rel, d, log)
		if ok && urlTooLong(f) {
			log.Warn("skipped", "path", rel, "reason", "over 256 bytes once escaped")
			return nil
		}
		if ok {
			out = append(out, f)
		}
		return nil
	})
	return out, err
}

// walkError passes over a file removed since its directory was listed: a
// directory changes while it is walked. Any other error stops the scan, so
// that an unreadable directory never reads as an empty one.
func walkError(top bool, err error) error {
	if !top && errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// skipped says whether the walk leaves a path out: a dotfile, such as an
// rsync temporary file or an editor's, or a match of an exclude pattern. A
// pattern with a slash matches the whole path, one without a name at any
// depth.
func skipped(rel string, exclude []string) bool {
	name := path.Base(rel)
	if strings.HasPrefix(name, ".") {
		return true
	}
	for _, pattern := range exclude {
		target := name
		if strings.Contains(pattern, "/") {
			target = rel
		}
		if ok, _ := path.Match(pattern, target); ok {
			return true
		}
	}
	return false
}

// proofs passes over a proofs directory, or a link to one: X.proofs/root
// is read with X, and clients find the shards beside it.
func proofs(abs, rel string, d fs.DirEntry, log *slog.Logger) error {
	if _, err := os.Stat(strings.TrimSuffix(abs, proofsDir)); errors.Is(err, fs.ErrNotExist) {
		log.Warn("skipped", "path", rel, "reason", "proofs without their file")
	}
	if d.IsDir() {
		return fs.SkipDir
	}
	return nil
}

// foundFile stats a file, following a symlink, and finds its descriptor.
func foundFile(abs, rel string, d fs.DirEntry, log *slog.Logger) (found, bool) {
	info, err := os.Stat(abs)
	reason := ""
	switch {
	case err != nil && d.Type()&fs.ModeSymlink != 0:
		reason = "broken symlink"
	case errors.Is(err, fs.ErrNotExist):
		return found{}, false
	case err != nil:
		reason = err.Error()
	case info.IsDir():
		reason = "symlinked directory, not followed"
	case !info.Mode().IsRegular():
		reason = "not a regular file"
	}
	root, pending := packedRoot(abs)
	if reason == "" && pending {
		reason = "its .proofs directory has no root yet"
	}
	if reason != "" {
		log.Warn("skipped", "path", rel, "reason", reason)
		return found{}, false
	}
	return found{rel: rel, abs: abs, at: stampOf(info), root: root}, true
}

// packedRoot finds X.proofs/root beside X. A proofs directory without a
// root is a packing or a copy under way, which X waits for.
func packedRoot(abs string) (root *stamp, pending bool) {
	if info, err := os.Stat(filepath.Join(abs+proofsDir, "root")); err == nil && info.Mode().IsRegular() {
		at := stampOf(info)
		return &at, false
	}
	info, err := os.Stat(abs + proofsDir)
	return nil, err == nil && info.IsDir()
}

func stampOf(info fs.FileInfo) stamp {
	return stamp{Size: info.Size(), MTime: info.ModTime().UnixNano()}
}

// urlTooLong says whether a found file's longest path, its descriptor's
// for a packed one, is past the index's bound.
func urlTooLong(f found) bool {
	n := len(escapePath(f.rel))
	if f.root != nil {
		n += len(proofsDir + "/root")
	}
	return n > maxURLBytes
}

// escapePath writes a path below the base as a URL path: each segment
// percent-encoded, ":" too, so that a first segment such as "a:b" cannot
// read as a scheme.
func escapePath(rel string) string {
	segments := strings.Split(rel, "/")
	for i, s := range segments {
		segments[i] = strings.ReplaceAll(url.PathEscape(s), ":", "%3A")
	}
	return strings.Join(segments, "/")
}

// identify names each found file by CID: a plain file by its bytes, read
// unless the cache knows them, and a packed one by its descriptor, which
// is published too, under the anchor.
func identify(found []found, cache *hashCache, log *slog.Logger) ([]file, error) {
	var out []file
	for _, f := range found {
		if f.root != nil {
			out = append(out, identifyPacked(f, log)...)
			continue
		}
		cid, ok, err := hashFile(f, cache, log)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, file{path: escapePath(f.rel), cid: cid, size: f.at.Size})
		}
	}
	return out, nil
}

// hashFile is a plain file's CID. A file that cannot be read stops the
// scan; one that changed while read is left for the next.
func hashFile(f found, cache *hashCache, log *slog.Logger) (string, bool, error) {
	if cid, ok := cache.get(f.abs, f.at); ok {
		return cid, true, nil
	}
	cid, n, err := fileCID(f.abs)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if n != f.at.Size {
		log.Warn("skipped", "path", f.rel, "reason", "changed while read")
		return "", false, nil
	}
	log.Debug("hashed", "path", f.rel)
	cache.put(f.abs, f.at, cid)
	return cid, true, nil
}

// identifyPacked publishes a packed file X at X and its descriptor at
// X.proofs/root, without reading X: when X's size is not the descriptor's,
// X is still being copied, or packed again, and both wait.
func identifyPacked(f found, log *slog.Logger) []file {
	p, err := readPacking(filepath.Join(f.abs+proofsDir, "root"))
	if err == nil && p.mapSize != f.at.Size {
		err = fmt.Errorf("%d bytes, but its descriptor says %d: a copy or a packing under way", f.at.Size, p.mapSize)
	}
	if err != nil {
		log.Warn("skipped", "path", f.rel, "reason", err.Error())
		return nil
	}
	if f.at.MTime > f.root.MTime {
		log.Warn("packed file newer than its descriptor", "path", f.rel)
	}
	path := escapePath(f.rel)
	return []file{
		{path: path + proofsDir + "/root", cid: p.anchor, size: p.rootSize},
		{path: path, cid: p.mapCID, size: p.mapSize},
	}
}

// entries groups files by CID, keeping the first 16 paths of each, as
// the index does.
func entries(files []file, log *slog.Logger) map[string]entry {
	byCID := map[string]entry{}
	for _, f := range files {
		e := byCID[f.cid]
		byCID[f.cid] = entry{urls: append(e.urls, f.path), size: f.size}
	}
	for cid, e := range byCID {
		slices.Sort(e.urls)
		if len(e.urls) > maxPaths {
			log.Warn("paths dropped", "cid", cid, "dropped", e.urls[maxPaths:])
			e.urls = e.urls[:maxPaths]
		}
		byCID[cid] = e
	}
	return byCID
}
