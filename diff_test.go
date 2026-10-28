package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

// in is testNow plus days, as the index writes times.
func in(days int) string {
	return testNow.Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
}

// storedOf reads a GET reply's section, as of testNow.
func storedOf(t *testing.T, section string) *stored {
	t.Helper()
	st := &stored{exists: section != "", now: testNow}
	if section == "" {
		return st
	}
	if err := json.Unmarshal([]byte(`{"offers":1,"section":`+section+`}`), st); err != nil {
		t.Fatal(err)
	}
	return st
}

func testSection(fields string) *Section {
	sec := &Section{Base: "https://tiles.example/"}
	if fields != "" {
		if err := json.Unmarshal([]byte(fields), sec); err != nil {
			panic(err)
		}
	}
	if err := sec.parseDurations(); err != nil {
		panic(err)
	}
	return sec
}

// diffCases are a section, its scan and what the index holds, and the
// patches and counts a sync makes of them.
const diffBase = `"base":"https://tiles.example/"`

var entryA = map[string]entry{"cidA": {urls: []string{"a.txt"}, size: 5}}
var diffCases = []struct {
	name    string
	sec     string
	scan    map[string]entry
	section string // as stored, "" for none
	bodies  []string
	counts  [3]int // added, changed, removed
	renewed bool
}{
	{"new section", ``, entryA, ``,
		[]string{`{` + diffBase + `,"hints":{"cidA":{"urls":["a.txt"],"size":5,"expires":"` + in(30) + `"}}}`}, [3]int{1, 0, 0}, false},
	{"no change", ``, entryA, `{` + diffBase + `,"hints":{"cidA":{"urls":["a.txt"],"size":5,"expires":"` + in(25) + `"}}}`,
		nil, [3]int{}, false},
	{"one added, one removed: the new CID lapses with the rest", ``,
		map[string]entry{"cidA": entryA["cidA"], "cidC": {urls: []string{"c.txt"}, size: 1}},
		`{` + diffBase + `,"hints":{"cidA":{"urls":["a.txt"],"size":5,"expires":"` + in(25) + `"},"cidB":{"urls":["b.txt"],"size":3,"expires":"` + in(26) + `"}}}`,
		[]string{`{"hints":{"cidB":null,"cidC":{"urls":["c.txt"],"size":1,"expires":"` + in(25) + `"}}}`}, [3]int{1, 0, 1}, false},
	{"paths changed", ``, map[string]entry{"cidA": {urls: []string{"a.txt", "b/a.txt"}, size: 5}},
		`{` + diffBase + `,"hints":{"cidA":{"urls":["a.txt"],"size":5,"expires":"` + in(25) + `"}}}`,
		[]string{`{"hints":{"cidA":{"urls":["a.txt","b/a.txt"],"size":5,"expires":"` + in(25) + `"}}}`}, [3]int{0, 1, 0}, false},
	{"size missing", ``, entryA, `{` + diffBase + `,"hints":{"cidA":{"urls":["a.txt"],"expires":"` + in(25) + `"}}}`,
		[]string{`{"hints":{"cidA":{"urls":["a.txt"],"size":5,"expires":"` + in(25) + `"}}}`}, [3]int{0, 1, 0}, false},
	{"a CID's own range cleared", ``, entryA,
		`{` + diffBase + `,"hints":{"cidA":{"urls":["a.txt"],"size":5,"expires":"` + in(25) + `","range":false}}}`,
		[]string{`{"hints":{"cidA":{"urls":["a.txt"],"size":5,"expires":"` + in(25) + `","range":null}}}`}, [3]int{0, 1, 0}, false},
	{"settings it does not know left alone", ``, entryA,
		`{` + diffBase + `,"x":1,"hints":{"cidA":{"urls":["a.txt"],"size":5,"expires":"` + in(25) + `","x":2}}}`,
		nil, [3]int{}, false},
	{"renewal due", ``, map[string]entry{"cidA": entryA["cidA"], "cidB": {urls: []string{"b.txt"}, size: 3}},
		`{` + diffBase + `,"hints":{"cidA":{"urls":["a.txt"],"size":5,"expires":"` + in(22) + `"},"cidB":{"urls":["b.txt"],"size":3,"expires":"` + in(29) + `"}}}`,
		[]string{`{"hints":{"cidA":{"urls":["a.txt"],"size":5,"expires":"` + in(30) + `"},"cidB":{"urls":["b.txt"],"size":3,"expires":"` + in(30) + `"}}}`}, [3]int{}, true},
	{"renewal not due a day earlier", ``, entryA, `{` + diffBase + `,"hints":{"cidA":{"urls":["a.txt"],"size":5,"expires":"` + in(23) + `"}}}`,
		nil, [3]int{}, false},
	{"settings changed", `{"base":"https://new.example/maps/","range":true}`, nil,
		`{"base":"https://tiles.example/"}`,
		[]string{`{"base":"https://new.example/maps/","range":true}`}, [3]int{}, false},
	{"settings dropped", `{"base":""}`, nil, `{"base":"eu/","parent":"europe","range":true}`,
		[]string{`{"base":null,"parent":null,"range":null}`}, [3]int{}, false},
}

func TestDiff(t *testing.T) {
	for _, tc := range diffCases {
		st := storedOf(t, tc.section)
		c := diff(testSection(tc.sec), tc.scan, st)
		got := c.bodies(st)
		if strings.Join(asStrings(got), "\n") != strings.Join(tc.bodies, "\n") {
			t.Errorf("%s: bodies\n%s\nwant\n%s", tc.name, asStrings(got), tc.bodies)
		}
		if counts := [3]int{c.added, c.changed, len(c.removed)}; counts != tc.counts || c.renewed != tc.renewed {
			t.Errorf("%s: added, changed, removed %v, renewed %v, want %v, %v", tc.name, counts, c.renewed, tc.counts, tc.renewed)
		}
	}
}

func asStrings(bodies [][]byte) []string {
	var out []string
	for _, b := range bodies {
		out = append(out, string(b))
	}
	return out
}

func TestGuard(t *testing.T) {
	st := storedOf(t, `{"hints":{"cidA":{"urls":["a"],"expires":"`+in(25)+`"},"cidB":{"urls":["b"],"expires":"`+in(25)+`"},"cidC":{"urls":["c"],"expires":"`+in(25)+`"}}}`)
	sec := testSection(``)
	keepOne := map[string]entry{"cidA": {urls: []string{"a"}}}
	keepTwo := map[string]entry{"cidA": {urls: []string{"a"}}, "cidB": {urls: []string{"b"}}}
	if err := guard(diff(sec, keepOne, st), st, false); err == nil || !strings.Contains(err.Error(), "-force") {
		t.Errorf("removing 2 of 3: guard = %v, want a refusal naming -force", err)
	}
	if err := guard(diff(sec, keepOne, st), st, true); err != nil {
		t.Errorf("removing 2 of 3 with -force: guard = %v", err)
	}
	if err := guard(diff(sec, keepTwo, st), st, false); err != nil {
		t.Errorf("removing 1 of 3: guard = %v", err)
	}
	replaced := map[string]entry{"cidD": {urls: []string{"a"}}, "cidE": {urls: []string{"b"}}, "cidF": {urls: []string{"c"}}}
	if err := guard(diff(sec, replaced, st), st, false); err != nil {
		t.Errorf("3 CIDs replaced by 3 others: guard = %v, want them through: the section does not shrink", err)
	}
}

func TestBatches(t *testing.T) {
	settings := map[string]any{"range": true}
	value := json.RawMessage(`{"urls":["` + strings.Repeat("x", 3<<20) + `"]}`)
	big := []patchItem{{cid: "cidA", value: value, urls: 1}, {cid: "cidB", value: value, urls: 1}, {cid: "cidC", value: value, urls: 1}}
	got := batches(settings, big)
	if len(got) != 2 || !bytes.HasPrefix(got[0], []byte(`{"hints":{"cidA":`)) || !bytes.Contains(got[0], []byte(`"range":true`)) || bytes.Contains(got[1], []byte("range")) {
		t.Errorf("3 MiB items: %d bodies, want 2, the settings in the first only", len(got))
	}
	for i, body := range got {
		if len(body) > maxBody {
			t.Errorf("body %d: %d bytes, over %d", i, len(body), maxBody)
		}
	}
	var many []patchItem
	for i := range maxURLs + 1 {
		many = append(many, patchItem{cid: fmt.Sprintf("cid%06d", i), value: json.RawMessage(`null`), urls: 1})
	}
	if got := batches(nil, many); len(got) != 2 {
		t.Errorf("%d one-URL items: %d bodies, want 2", maxURLs+1, len(got))
	}
	if got := batches(nil, nil); len(got) != 0 {
		t.Errorf("nothing: %d bodies, want none", len(got))
	}
}
