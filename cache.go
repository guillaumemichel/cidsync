package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// A hashCache remembers each file's CID by its absolute path, size and
// modification time, so that a file is read once until it changes. It is
// only a cache: a missing or unreadable one costs a read of every file.
type hashCache struct {
	file  string            // where it is saved, or "" for nowhere
	files map[string]cached // by absolute path
	seen  map[string]bool   // the paths scanned since the last save
	dirty bool
}

type cached struct {
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
	CID   string `json:"cid"`
}

// A stamp is what tells a file changed without reading it.
type stamp struct{ Size, MTime int64 }

func loadCache(file string) *hashCache {
	c := &hashCache{file: file, files: map[string]cached{}, seen: map[string]bool{}}
	b, err := os.ReadFile(file)
	if err != nil || json.Unmarshal(b, &c.files) != nil || c.files == nil {
		c.files = map[string]cached{}
	}
	return c
}

// cachePath is where the hash cache lives: systemd's CacheDirectory when
// the service has one, else the user's cache directory.
func cachePath(getenv func(string) string) string {
	if dir, _, _ := strings.Cut(getenv("CACHE_DIRECTORY"), ":"); dir != "" {
		return filepath.Join(dir, "hashes.json")
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "cidsync", "hashes.json")
}

func (c *hashCache) get(abs string, at stamp) (string, bool) {
	c.seen[abs] = true
	e, ok := c.files[abs]
	return e.CID, ok && e.Size == at.Size && e.MTime == at.MTime
}

func (c *hashCache) put(abs string, at stamp, cid string) {
	c.seen[abs] = true
	c.files[abs] = cached{Size: at.Size, MTime: at.MTime, CID: cid}
	c.dirty = true
}

// save drops the files below dirs that no scan saw since the last save,
// and writes the cache when it changed. Files elsewhere are another
// config's.
func (c *hashCache) save(dirs []string) error {
	for abs := range c.files {
		if !c.seen[abs] && below(abs, dirs) {
			delete(c.files, abs)
			c.dirty = true
		}
	}
	c.seen = map[string]bool{}
	if !c.dirty || c.file == "" {
		return nil
	}
	b, err := json.Marshal(c.files)
	if err != nil {
		return err
	}
	if err := writeAtomic(c.file, b); err != nil {
		return err
	}
	c.dirty = false
	return nil
}

func below(abs string, dirs []string) bool {
	for _, dir := range dirs {
		if strings.HasPrefix(abs, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// writeAtomic writes a file whole or not at all: a temporary file beside
// it, renamed over it.
func writeAtomic(file string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".hashes-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}
