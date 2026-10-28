package main

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "0123456789abcdef0123456789abcdef"

// routes serves requests in memory, with a handler per host: no network,
// so a synctest bubble sees every wait.
type routes map[string]http.Handler

func (r routes) RoundTrip(req *http.Request) (*http.Response, error) {
	h, ok := r[req.URL.Host]
	if !ok {
		return nil, fmt.Errorf("dial %s: connection refused", req.URL.Host)
	}
	// A server's request always has a body; a client's GET has none.
	req = req.Clone(req.Context())
	if req.Body == nil {
		req.Body = http.NoBody
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// A fakeIndex holds one account's sections in memory and serves them as
// the index does: GET, and PATCH as a JSON merge patch, which creates a
// missing section.
type fakeIndex struct {
	mu       sync.Mutex
	now      time.Time
	sections map[string]map[string]any // by name, "" for the catalog
	requests []fakeRequest
	fail     int               // the next requests get a 503
	refuse   map[string]string // sections whose patches get a 400, with this detail
}

type fakeRequest struct {
	method, path, body string
}

func newFakeIndex() *fakeIndex {
	return &fakeIndex{now: testNow, sections: map[string]map[string]any{"": {}}}
}

// seed stores a section document as given.
func (f *fakeIndex) seed(t *testing.T, name, doc string) {
	t.Helper()
	var section map[string]any
	if err := json.Unmarshal([]byte(doc), &section); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sections[name] = section
}

func (f *fakeIndex) section(name string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sections[name]
}

// patches lists the bodies of the PATCH requests to a section.
func (f *fakeIndex) patches(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		if r.method == http.MethodPatch && r.path == sectionPath(name) {
			out = append(out, r.body)
		}
	}
	return out
}

func (f *fakeIndex) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	f.requests = append(f.requests, fakeRequest{r.Method, r.URL.Path, string(body)})
	now := f.now
	if now.IsZero() {
		now = time.Now() // a synctest bubble's clock
	}
	w.Header().Set("Date", now.UTC().Format(http.TimeFormat))
	name, known := strings.CutPrefix(r.URL.Path, "/sections/")
	if r.URL.Path == "/catalog" {
		name, known = "", true
	}
	switch {
	case f.fail > 0:
		f.fail--
		reply(w, http.StatusServiceUnavailable, map[string]any{"error": "internal"})
	case r.Header.Get("Authorization") != "Bearer "+testKey:
		reply(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
	case !known:
		reply(w, http.StatusNotFound, map[string]any{"error": "not_found"})
	case r.Method == http.MethodGet:
		f.get(w, name)
	case r.Method == http.MethodPatch:
		f.patch(w, name, body)
	default:
		reply(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
	}
}

func (f *fakeIndex) get(w http.ResponseWriter, name string) {
	section, ok := f.sections[name]
	if !ok {
		reply(w, http.StatusNotFound, map[string]any{"error": "unknown_section"})
		return
	}
	reply(w, http.StatusOK, map[string]any{"offers": f.offers(), "section": section, "effective": f.effective(name)})
}

func (f *fakeIndex) patch(w http.ResponseWriter, name string, body []byte) {
	var patch map[string]any
	if detail, ok := f.refuse[name]; ok {
		reply(w, http.StatusBadRequest, map[string]any{"error": "invalid_catalog", "detail": detail})
		return
	}
	if len(body) > 8<<20 {
		reply(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "body_too_large"})
		return
	}
	if err := json.Unmarshal(body, &patch); err != nil {
		reply(w, http.StatusBadRequest, map[string]any{"error": "invalid_catalog", "detail": "not a JSON object"})
		return
	}
	merged := mergePatch(f.sections[name], patch).(map[string]any)
	hints, _ := merged["hints"].(map[string]any)
	for cid, e := range hints {
		if _, ok := e.(map[string]any)["urls"]; !ok {
			reply(w, http.StatusBadRequest, map[string]any{"error": "invalid_catalog", "detail": "hints." + cid + ": urls: required"})
			return
		}
	}
	if len(hints) == 0 {
		delete(merged, "hints")
	}
	f.sections[name] = merged
	reply(w, http.StatusOK, map[string]any{"offers": f.offers()})
}

// mergePatch applies an RFC 7396 merge patch.
func mergePatch(target, patch any) any {
	p, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	out, _ := target.(map[string]any)
	out = maps.Clone(out)
	if out == nil {
		out = map[string]any{}
	}
	for k, v := range p {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = mergePatch(out[k], v)
	}
	return out
}

func (f *fakeIndex) offers() int {
	n := 0
	for _, section := range f.sections {
		hints, _ := section["hints"].(map[string]any)
		n += len(hints)
	}
	return n
}

// effective is what a section's CIDs get: the bases from the catalog
// down, resolved, and the nearest range.
func (f *fakeIndex) effective(name string) map[string]any {
	chain := []map[string]any{f.sections[name]}
	for name != "" {
		name, _ = f.sections[name]["parent"].(string)
		chain = append([]map[string]any{f.sections[name]}, chain...)
	}
	out := map[string]any{}
	var base *url.URL
	for _, level := range chain {
		if b, ok := level["base"].(string); ok {
			ref, _ := url.Parse(b)
			if base != nil {
				ref = base.ResolveReference(ref)
			}
			base, out["base"] = ref, ref.String()
		}
		if v, ok := level["range"]; ok {
			out["range"] = v
		}
	}
	return out
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// testClient is a client of the index at https://index.test, with the
// test key.
func testClient(index http.Handler) *client {
	return &client{url: "https://index.test", key: testKey, http: &http.Client{Transport: routes{"index.test": index}}}
}
