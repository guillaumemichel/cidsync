package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// TestCacheSparesRead swaps a file's bytes for others of the same size and
// time: a scan that reads the file would see them.
func TestCacheSparesRead(t *testing.T) {
	dir, cacheFile := t.TempDir(), filepath.Join(t.TempDir(), "sub", "hashes.json")
	hello := filepath.Join(dir, "hello.txt")
	writeFile(t, hello, []byte("hello\n"))
	log, _ := newTestLogger()
	sec := &Section{Dir: dir}
	cache := loadCache(cacheFile)
	if _, err := scanSection(sec, cache, log); err != nil {
		t.Fatal(err)
	}
	if err := cache.save([]string{dir}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(hello)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, hello, []byte("jello\n"))
	if err := os.Chtimes(hello, time.Time{}, info.ModTime()); err != nil {
		t.Fatal(err)
	}
	for name, cache := range map[string]*hashCache{"in memory": cache, "from its file": loadCache(cacheFile)} {
		got, err := scanSection(sec, cache, log)
		if err != nil || got[helloRaw].size != 6 {
			t.Errorf("%s: scan = %v, %v, want the cached CID", name, got, err)
		}
	}
	if err := os.WriteFile(cacheFile, []byte(`{"`+hello+`": {"size": "six"`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := scanSection(sec, loadCache(cacheFile), log)
	if err != nil || len(got) != 1 || got[helloRaw].size != 0 {
		t.Errorf("corrupt cache: scan = %v, %v, want the file read again", got, err)
	}
}

func TestCachePrunes(t *testing.T) {
	dir, cacheFile := t.TempDir(), filepath.Join(t.TempDir(), "hashes.json")
	writeFile(t, filepath.Join(dir, "a.txt"), []byte("a"))
	writeFile(t, filepath.Join(dir, "b.txt"), []byte("b"))
	elsewhere := filepath.Join(t.TempDir(), "kept.txt")
	cache := loadCache(cacheFile)
	cache.put(elsewhere, stamp{Size: 1}, helloRaw)
	log, _ := newTestLogger()
	sec := &Section{Dir: dir}
	// A run scans every section, then saves.
	for range 2 {
		if _, err := scanSection(sec, cache, log); err != nil {
			t.Fatal(err)
		}
		if err := cache.save([]string{dir}); err != nil {
			t.Fatal(err)
		}
		os.Remove(filepath.Join(dir, "b.txt"))
	}
	var paths []string
	for path := range loadCache(cacheFile).files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	if want := []string{filepath.Join(dir, "a.txt"), elsewhere}; !slices.Equal(paths, want) && !slices.Equal(paths, []string{want[1], want[0]}) {
		t.Errorf("cache holds %v, want %v: b.txt pruned, a path outside the scan kept", paths, want)
	}
}

func TestCachePath(t *testing.T) {
	env := map[string]string{"CACHE_DIRECTORY": "/var/cache/cidsync:/var/cache/other"}
	if got := cachePath(func(k string) string { return env[k] }); got != "/var/cache/cidsync/hashes.json" {
		t.Errorf("cachePath = %q, want systemd's first cache directory", got)
	}
	if got := cachePath(func(string) string { return "" }); filepath.Base(got) != "hashes.json" {
		t.Errorf("cachePath = %q, want a file in the user's cache directory", got)
	}
}
