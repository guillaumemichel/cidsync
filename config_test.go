package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig writes doc as a config file beside a directory "files" and
// a plain file "file", and returns its path.
func writeConfig(t *testing.T, doc string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "publish.json")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	path := writeConfig(t, `{
		"index": "https://index.example/",
		"sections": {
			"files": {"dir": "files", "base": "https://tiles.example/", "range": true, "exclude": ["*.tmp", "drafts/*"]},
			"long": {"dir": "files", "base": "https://long.example/", "lifetime": "60d", "renew": "1d12h"},
			"": {"dir": "files", "base": "https://tiles.example/root/"}
		}
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Index != "https://index.example" {
		t.Errorf("index = %q, want the URL without its trailing slash", cfg.Index)
	}
	files, long := cfg.Sections["files"], cfg.Sections["long"]
	if want := filepath.Join(filepath.Dir(path), "files"); files.Dir != want {
		t.Errorf("dir = %q, want %q, relative to the config file", files.Dir, want)
	}
	if files.lifetime != 30*24*time.Hour || files.renew != 7*24*time.Hour {
		t.Errorf("defaults: lifetime %v, renew %v, want 720h and 168h", files.lifetime, files.renew)
	}
	if long.lifetime != 60*24*time.Hour || long.renew != 36*time.Hour {
		t.Errorf("long: lifetime %v, renew %v, want 1440h and 36h", long.lifetime, long.renew)
	}
	if files.Range == nil || !*files.Range || long.Range != nil {
		t.Errorf("range: files %v, long %v, want true and unset", files.Range, long.Range)
	}
	if got := sectionNames(cfg); strings.Join(got, ",") != ",files,long" {
		t.Errorf("sectionNames = %q, want the catalog first, then by name", got)
	}
}

func TestLoadConfigLoopbackHTTP(t *testing.T) {
	for _, index := range []string{"http://127.0.0.1:8090", "http://localhost:8090", "http://[::1]:8090"} {
		path := writeConfig(t, `{"index": "`+index+`", "sections": {"a": {"dir": "files"}}}`)
		if _, err := loadConfig(path); err != nil {
			t.Errorf("%s: %v, want plain http allowed to a loopback host", index, err)
		}
	}
}

func TestLoadConfigErrors(t *testing.T) {
	const index = `"index": "https://index.example"`
	section := func(fields string) string {
		return `{` + index + `, "sections": {"a": {"dir": "files"` + fields + `}}}`
	}
	tests := []struct {
		name, doc, want string
	}{
		{"not JSON", `{`, "publish.json"},
		{"trailing data", section(``) + `{}`, "trailing data"},
		{"unknown field", section(`, "bsae": "x/"`), "bsae"},
		{"no index", `{"sections": {"a": {"dir": "files"}}}`, "index: required"},
		{"index plain http", `{"index": "http://index.example", "sections": {"a": {"dir": "files"}}}`, "https"},
		{"index with a query", `{"index": "https://index.example/?x", "sections": {"a": {"dir": "files"}}}`, "query"},
		{"no sections", `{` + index + `}`, "sections: none"},
		{"bad section name", `{` + index + `, "sections": {"Files": {"dir": "files"}}}`, `"Files"`},
		{"section name with a leading dash", `{` + index + `, "sections": {"-a": {"dir": "files"}}}`, `"-a"`},
		{"section name too long", `{` + index + `, "sections": {"` + strings.Repeat("a", 65) + `": {"dir": "files"}}}`, "name"},
		{"no dir", `{` + index + `, "sections": {"a": {}}}`, "dir: required"},
		{"missing dir", `{` + index + `, "sections": {"a": {"dir": "nowhere"}}}`, "nowhere"},
		{"dir a file", `{` + index + `, "sections": {"a": {"dir": "file"}}}`, "not a directory"},
		{"base without a slash", section(`, "base": "https://tiles.example/maps"`), "end with /"},
		{"base plain http", section(`, "base": "http://tiles.example/"`), "https"},
		{"base from the host root", section(`, "base": "/maps/"`), "relative"},
		{"catalog without a base", `{` + index + `, "sections": {"": {"dir": "files"}}}`, "absolute"},
		{"catalog with a relative base", `{` + index + `, "sections": {"": {"dir": "files", "base": "eu/"}}}`, "absolute"},
		{"catalog with a parent", `{` + index + `, "sections": {"": {"dir": "files", "base": "https://t.example/", "parent": "a"}}}`, "parent"},
		{"own parent", section(`, "parent": "a"`), "parent"},
		{"bad parent name", section(`, "parent": "A"`), "parent"},
		{"bad duration", section(`, "lifetime": "3x"`), "lifetime"},
		{"duration not a string", section(`, "lifetime": 30`), "lifetime"},
		{"lifetime past a year", section(`, "lifetime": "366d"`), "365d"},
		{"negative renew", section(`, "renew": "-1h"`), "renew"},
		{"renew not below lifetime", section(`, "lifetime": "7d", "renew": "7d"`), "renew"},
		{"bad exclude pattern", section(`, "exclude": ["["]`), "exclude"},
	}
	for _, tc := range tests {
		_, err := loadConfig(writeConfig(t, tc.doc))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one mentioning %q", tc.name, err, tc.want)
		}
	}
}

func TestSectionNamesParentsFirst(t *testing.T) {
	cfg := &Config{Sections: map[string]*Section{
		"a": {Parent: "b"}, "b": {Parent: "c"}, "c": {}, "d": {}, "": {},
		"loop1": {Parent: "loop2"}, "loop2": {Parent: "loop1"}, // the index refuses a cycle
	}}
	if got := strings.Join(sectionNames(cfg), ","); got != ",c,d,b,a,loop1,loop2" {
		t.Errorf("sectionNames = %q, want the catalog, then each parent before its children", got)
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"30d", 720 * time.Hour},
		{"1d12h", 36 * time.Hour},
		{"90m", 90 * time.Minute},
		{"0d", 0},
	}
	for _, tc := range tests {
		got, err := parseDuration(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parseDuration(%q) = %v, %v, want %v", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"", "d", "1.5d", "x1d", "1d2", "-1d"} {
		if got, err := parseDuration(in); err == nil {
			t.Errorf("parseDuration(%q) = %v, want an error", in, got)
		}
	}
}

func TestSectionPath(t *testing.T) {
	if got := sectionPath(""); got != "/catalog" {
		t.Errorf(`sectionPath("") = %q, want /catalog`, got)
	}
	if got := sectionPath("files"); got != "/sections/files" {
		t.Errorf(`sectionPath("files") = %q, want /sections/files`, got)
	}
}
