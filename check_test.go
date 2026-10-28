package main

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

// A recorder notes the requests a host gets.
type recorder struct {
	mu    sync.Mutex
	got   []*http.Request
	serve http.Handler
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.got = append(r.got, req)
	r.mu.Unlock()
	r.serve.ServeHTTP(w, req)
}

func (r *recorder) paths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, req := range r.got {
		out = append(out, req.URL.EscapedPath())
	}
	slices.Sort(out)
	return out
}

func TestCheckPasses(t *testing.T) {
	h := newHarness(t, filesSection)
	files := &recorder{serve: h.hosts["files.test"]}
	h.hosts["files.test"] = files
	h.write("hello.txt", "hello\n")
	h.write("a b/c:d.txt", "hello\n")
	h.write("empty", "")
	if code := h.sync(); code != 0 {
		t.Fatalf("exit %d, %s", code, h.logs)
	}
	if got := h.hint("files", helloRaw); !strings.Contains(got, `"urls":["a%20b/c%3Ad.txt","hello.txt"]`) {
		t.Errorf("published %s, want both paths", got)
	}
	if got := files.paths(); !slices.Equal(got, []string{"/a%20b/c%3Ad.txt", "/empty", "/hello.txt"}) {
		t.Errorf("checked %v, want each URL once", got)
	}
	for _, req := range files.got {
		ranged := req.Header.Get("Range") == "bytes=0-0"
		if req.Header.Get("Origin") == "" || ranged == (req.URL.Path == "/empty") {
			t.Errorf("%s: Origin %q, Range %q: want an Origin, and one byte of each file that has one", req.URL, req.Header.Get("Origin"), req.Header.Get("Range"))
		}
		if req.Header.Get("Authorization") != "" {
			t.Errorf("%s: an Authorization header: the index's key must not reach a file host", req.URL)
		}
	}
	h.sync()
	if got := len(files.paths()); got != 3 {
		t.Errorf("%d checks after a sync with nothing new, want the first 3 only", got)
	}
}

func TestCheckFails(t *testing.T) {
	h := newHarness(t, filesSection)
	h.write("hello.txt", "hello\n")
	h.sync()
	h.write("copy.txt", "hello\n") // a new URL for a CID the index holds
	h.write("new.txt", "new")
	h.hosts["files.test"] = fileHost(t.TempDir()) // which serves neither
	if code := h.sync(); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if got := h.hint("files", helloRaw); !strings.Contains(got, `"urls":["hello.txt"]`) {
		t.Errorf("held %s, want hello.txt only: copy.txt failed its check", got)
	}
	if got := h.hint("files", sumCID(codecRaw, []byte("new"))); got != "" {
		t.Errorf("held %s for new.txt, want nothing", got)
	}
	if len(h.logs.lines("check failed")) != 2 {
		t.Errorf("logged %s, want two failed checks", h.logs)
	}
	if s := h.synced("files"); s["unchecked"] != 2.0 {
		t.Errorf("synced line %v, want 2 unchecked", s)
	}
}

func TestCheckExpectations(t *testing.T) {
	ok := func(status int, acao bool) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if acao {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			}
			w.WriteHeader(status)
		})
	}
	tests := []struct {
		name, section string
		host          http.Handler
		published     bool
		warned        bool
	}{
		{"200 where ranges are declared", filesSection, ok(http.StatusOK, true), false, false},
		{"200 where they are not", `{"files": {"dir": "DIR", "base": "https://files.test/"}}`, ok(http.StatusOK, true), true, false},
		{"no Access-Control-Allow-Origin", filesSection, ok(http.StatusPartialContent, false), true, true},
		{"404", filesSection, ok(http.StatusNotFound, true), false, false},
	}
	for _, tc := range tests {
		h := newHarness(t, tc.section)
		h.hosts["files.test"] = tc.host
		h.write("hello.txt", "hello\n")
		h.sync()
		published := h.hint("files", helloRaw) != ""
		warned := len(h.logs.lines("check warning")) > 0
		if published != tc.published || warned != tc.warned {
			t.Errorf("%s: published %v, warned %v, want %v, %v", tc.name, published, warned, tc.published, tc.warned)
		}
	}
}

func TestCheckRelativeBase(t *testing.T) {
	h := newHarness(t, `{"maps": {"dir": "DIR/maps", "base": "maps/"}}`)
	h.index.seed(t, "", `{"base":"https://files.test/"}`)
	h.write("maps/hello.txt", "hello\n")
	files := &recorder{serve: fileHost(h.dir)}
	h.hosts["files.test"] = files
	if code := h.sync(); code != 0 {
		t.Fatalf("exit %d, %s", code, h.logs)
	}
	if got := files.paths(); !slices.Equal(got, []string{"/maps/hello.txt"}) {
		t.Errorf("checked %v, want the path below the catalog's base", got)
	}
	catalogReads := func() (n int) {
		for _, r := range h.index.requests {
			if r.path == "/catalog" {
				n++
			}
		}
		return n
	}
	before := catalogReads()
	h.write("maps/new.txt", "new")
	if code := h.sync(); code != 0 || catalogReads() != before {
		t.Errorf("exit %d, %d reads of the catalog, want 0 and none: the section's own effective base serves", code, catalogReads()-before)
	}
}
