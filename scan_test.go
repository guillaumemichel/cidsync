package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// scanOf scans dir as a section excluding exclude, with a cache in its own
// temporary directory.
func scanOf(t *testing.T, dir string, exclude ...string) (map[string]entry, *logBuffer) {
	t.Helper()
	log, logs := newTestLogger()
	cache := loadCache(filepath.Join(t.TempDir(), "hashes.json"))
	got, err := scanSection(&Section{Dir: dir, Exclude: exclude}, cache, log)
	if err != nil {
		t.Fatal(err)
	}
	return got, logs
}

// packNumbers writes numbers.txt and its packing below dir/sub.
func packNumbers(t *testing.T, dir, sub string) {
	t.Helper()
	root, err := os.ReadFile(numbersRoot)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, sub, "numbers.txt"), numbers())
	writeFile(t, filepath.Join(dir, sub, "numbers.txt.proofs", "0"), []byte("a shard"))
	writeFile(t, filepath.Join(dir, sub, "numbers.txt.proofs", "root"), root)
}

func TestScanPlainAndPacked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "hello.txt"), []byte("hello\n"))
	packNumbers(t, dir, "maps")
	got, logs := scanOf(t, dir)
	want := map[string]entry{
		helloRaw:      {urls: []string{"hello.txt"}, size: 6},
		numbersAnchor: {urls: []string{"maps/numbers.txt.proofs/root"}, size: 110},
		numbersMap:    {urls: []string{"maps/numbers.txt"}, size: numbersSize},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scan = %v, want %v", got, want)
	}
	if skipped := skippedPaths(t, logs); len(skipped) > 0 {
		t.Errorf("skipped %v, want nothing", skipped)
	}
}

func TestScanSkips(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".hidden", ".numbers.txt.Xy12ab", ".git/config", ".rsync-partial/big.bin",
		"a.tmp", "sub/b.tmp", "drafts/x.txt", "drafts/deeper/y.txt"} {
		writeFile(t, filepath.Join(dir, name), []byte(name))
	}
	writeFile(t, filepath.Join(dir, "sub/drafts/kept.txt"), []byte("hello\n"))
	// A pattern with a slash matches the path, one without a name at any depth.
	got, _ := scanOf(t, dir, "*.tmp", "drafts/*")
	want := map[string]entry{helloRaw: {urls: []string{"sub/drafts/kept.txt"}, size: 6}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scan = %v, want only sub/drafts/kept.txt: dotfiles and exclusions left out", got)
	}
	got, _ = scanOf(t, dir, "*.tmp", "drafts")
	if len(got) != 0 {
		t.Errorf("excluding drafts: scan = %v, want nothing", got)
	}
}

func TestScanSymlinks(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(outside, "hello.txt"), []byte("hello\n"))
	writeFile(t, filepath.Join(outside, "sub", "other.txt"), []byte("other"))
	for link, target := range map[string]string{
		"linked.txt": filepath.Join(outside, "hello.txt"),
		"broken.txt": filepath.Join(outside, "nowhere"),
		"linkdir":    filepath.Join(outside, "sub"),
	} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	got, logs := scanOf(t, dir)
	want := map[string]entry{helloRaw: {urls: []string{"linked.txt"}, size: 6}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scan = %v, want the linked file only", got)
	}
	if skipped := skippedPaths(t, logs); !slices.Equal(skipped, []string{"broken.txt", "linkdir"}) {
		t.Errorf("skipped %v, want the broken link and the linked directory", skipped)
	}
}

func TestScanLinkedProofs(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	packNumbers(t, outside, "")
	writeFile(t, filepath.Join(dir, "numbers.txt"), numbers())
	if err := os.Symlink(filepath.Join(outside, "numbers.txt.proofs"), filepath.Join(dir, "numbers.txt.proofs")); err != nil {
		t.Fatal(err)
	}
	got, logs := scanOf(t, dir)
	if len(got) != 2 || got[numbersAnchor].size != 110 {
		t.Errorf("scan = %v, want the anchor and the map", got)
	}
	if skipped := skippedPaths(t, logs); len(skipped) > 0 {
		t.Errorf("skipped %v, want nothing: a linked proofs directory is read with its file", skipped)
	}
}

func TestScanPackedPending(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  map[string]entry
	}{
		{"file shorter than mapSize", func(t *testing.T, dir string) {
			packNumbers(t, dir, "")
			writeFile(t, filepath.Join(dir, "numbers.txt"), numbers()[:1000])
		}, map[string]entry{}},
		{"proofs without their file", func(t *testing.T, dir string) {
			packNumbers(t, dir, "")
			os.Remove(filepath.Join(dir, "numbers.txt"))
		}, map[string]entry{}},
		{"proofs without a root", func(t *testing.T, dir string) {
			packNumbers(t, dir, "")
			os.Remove(filepath.Join(dir, "numbers.txt.proofs", "root"))
		}, map[string]entry{}},
		{"a descriptor that is not one", func(t *testing.T, dir string) {
			packNumbers(t, dir, "")
			writeFile(t, filepath.Join(dir, "numbers.txt.proofs", "root"), []byte("hello\n"))
		}, map[string]entry{}},
	}
	for _, tc := range tests {
		dir := t.TempDir()
		tc.setup(t, dir)
		got, logs := scanOf(t, dir)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: scan = %v, want %v", tc.name, got, tc.want)
		}
		if len(skippedPaths(t, logs)) != 1 {
			t.Errorf("%s: logged %s, want one skipped line", tc.name, logs)
		}
	}
}

func TestScanPackedNewerWarns(t *testing.T) {
	dir := t.TempDir()
	packNumbers(t, dir, "")
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "numbers.txt"), later, later); err != nil {
		t.Fatal(err)
	}
	got, logs := scanOf(t, dir)
	if len(got) != 2 {
		t.Errorf("scan = %v, want the anchor and the map still", got)
	}
	if len(logs.lines("packed file newer than its descriptor")) != 1 {
		t.Errorf("logged %s, want a warning", logs)
	}
}

func TestScanKeepsSixteenPaths(t *testing.T) {
	dir := t.TempDir()
	for i := range 17 {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("copy%02d.txt", i)), []byte("hello\n"))
	}
	got, logs := scanOf(t, dir)
	urls := got[helloRaw].urls
	if len(urls) != 16 || urls[0] != "copy00.txt" || urls[15] != "copy15.txt" {
		t.Errorf("urls = %v, want copy00.txt to copy15.txt", urls)
	}
	if len(logs.lines("paths dropped")) != 1 {
		t.Errorf("logged %s, want the dropped path told", logs)
	}
}

func TestScanEscapes(t *testing.T) {
	dir := t.TempDir()
	names := map[string]string{
		"a b": "a%20b", "c#d": "c%23d", "e?f": "e%3Ff", "g%h": "g%25h", "i:j": "i%3Aj", "é": "%C3%A9",
		"k;l": "k%3Bl", "m+n": "m+n", "sub dir/o:p": "sub%20dir/o%3Ap",
	}
	for name := range names {
		writeFile(t, filepath.Join(dir, name), []byte("hello\n"))
	}
	long := strings.Repeat("x", 200) + "/" + strings.Repeat("y", 50) + "é" // 257 bytes once escaped
	writeFile(t, filepath.Join(dir, long), []byte("hello\n"))
	got, logs := scanOf(t, dir)
	want := slices.Sorted(maps.Values(names))
	if urls := got[helloRaw].urls; !slices.Equal(urls, want) {
		t.Errorf("urls = %q, want %q", urls, want)
	}
	if skipped := skippedPaths(t, logs); !slices.Equal(skipped, []string{long}) {
		t.Errorf("skipped %v, want the path over 256 bytes", skipped)
	}
}

func TestScanMissingDir(t *testing.T) {
	cache := loadCache("")
	log, _ := newTestLogger()
	if _, err := scanSection(&Section{Dir: filepath.Join(t.TempDir(), "gone")}, cache, log); err == nil {
		t.Error("scan of a missing directory succeeded, want an error: never an empty section")
	}
}
