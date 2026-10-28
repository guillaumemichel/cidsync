package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"
)

const (
	timeFormat = time.RFC3339 // in UTC, to the second, as the index keeps it
	maxBody    = 7 << 20      // per patch; the index takes 8 MiB
	maxURLs    = 65_536       // per patch, as the index
)

// A stored section is a section as the index holds it: a GET reply, and
// the reply's Date, the index's clock, which every expiry is set by.
type stored struct {
	exists    bool
	now       time.Time
	Offers    int           `json:"offers"`
	Section   storedSection `json:"section"`
	Effective settings      `json:"effective"`
}

type storedSection struct {
	settings
	Parent string                 `json:"parent"`
	Hints  map[string]storedEntry `json:"hints"`
}

type settings struct {
	Base  string `json:"base"`
	Range *bool  `json:"range"`
}

type storedEntry struct {
	URLs    []string  `json:"urls"`
	Size    *int64    `json:"size"`
	Expires time.Time `json:"expires"`
	Range   *bool     `json:"range"`
}

// A change is what a sync writes to a section.
type change struct {
	settings map[string]any   // the settings that differ; nil to drop one
	removed  []string         // sorted
	sent     map[string]entry // added, changed, or renewed
	expires  time.Time        // every sent CID's
	renewed  bool
	added    int
	changed  int
}

// diff compares a section's scan with what the index holds of it.
func diff(sec *Section, scan map[string]entry, st *stored) change {
	expires, renewed := expiry(sec, st)
	c := change{settings: settingsDiff(sec, st.Section), sent: map[string]entry{}, expires: expires, renewed: renewed}
	for cid := range st.Section.Hints {
		if _, ok := scan[cid]; !ok {
			c.removed = append(c.removed, cid)
		}
	}
	slices.Sort(c.removed)
	for cid, e := range scan {
		old, ok := st.Section.Hints[cid]
		switch {
		case !ok:
			c.added++
		case !old.same(e):
			c.changed++
		case !renewed:
			continue
		}
		c.sent[cid] = e
	}
	return c
}

// expiry is the expiry of every CID a sync sends, and whether the sync
// renews the section. Until a renewal is due, it is the earliest stored
// expiry, so that a new CID lapses with the rest; a renewal is due renew
// after the last, and sends every CID with now + lifetime.
func expiry(sec *Section, st *stored) (time.Time, bool) {
	renewed := st.now.Add(sec.lifetime)
	if len(st.Section.Hints) == 0 {
		return renewed, false
	}
	var earliest time.Time
	for _, e := range st.Section.Hints {
		if earliest.IsZero() || e.Expires.Before(earliest) {
			earliest = e.Expires
		}
	}
	if earliest.Sub(st.now) < sec.lifetime-sec.renew {
		return renewed, true
	}
	return earliest, false
}

// same says whether the index holds e as the scan found it: the same
// paths, the same size, and no range of the CID's own.
func (s storedEntry) same(e entry) bool {
	return slices.Equal(s.URLs, e.urls) && s.Size != nil && *s.Size == e.size && s.Range == nil
}

// settingsDiff is what a patch sets of the section's settings: each one
// that differs from the stored one, and null for one the config drops.
func settingsDiff(sec *Section, old storedSection) map[string]any {
	out := map[string]any{}
	if sec.Base != old.Base {
		out["base"] = orNull(sec.Base)
	}
	if sec.Parent != old.Parent {
		out["parent"] = orNull(sec.Parent)
	}
	if !equal(sec.Range, old.Range) {
		out["range"] = sec.Range
	}
	return out
}

func orNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func equal[T comparable](a, b *T) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// guard refuses a sync that would leave a section with fewer than half of
// its CIDs: an unmounted or emptied directory looks like that. CIDs
// replaced by others, as when files change, do not count.
func guard(c change, st *stored, force bool) error {
	held := len(st.Section.Hints)
	left := held - len(c.removed) + c.added
	if force || 2*left >= held {
		return nil
	}
	return fmt.Errorf("would leave %d of its %d CIDs; pass -force if that is meant", left, held)
}

// A hintPatch is a CID's entry in a patch. It is always an object: a URL
// list would also drop the CID's own settings, and give it 30 days
// whatever the section's expiry.
type hintPatch struct {
	URLs    []string        `json:"urls"`
	Size    *int64          `json:"size,omitempty"`
	Expires string          `json:"expires"`
	Range   json.RawMessage `json:"range,omitempty"`
}

var null = json.RawMessage("null")

// A patchItem is a CID's entry in a patch, encoded.
type patchItem struct {
	cid   string
	value json.RawMessage
	urls  int
}

// bodies are the change's patches: removals first, then what it sends,
// each sorted, so the same change always gives the same bytes.
func (c change) bodies(st *stored) [][]byte {
	var items []patchItem
	for _, cid := range c.removed {
		items = append(items, patchItem{cid: cid, value: null})
	}
	for _, cid := range slices.Sorted(maps.Keys(c.sent)) {
		e := c.sent[cid]
		h := hintPatch{URLs: e.urls, Size: &e.size, Expires: c.expires.UTC().Format(timeFormat)}
		if old, ok := st.Section.Hints[cid]; ok && old.Range != nil {
			h.Range = null
		}
		items = append(items, patchItem{cid: cid, value: mustJSON(h), urls: len(e.urls)})
	}
	return batches(c.settings, items)
}

// batches packs patch items into bodies under the index's bounds. The
// settings go in the first, with the first items: the removals, so that
// the offers they free count before the next bodies add theirs.
func batches(settings map[string]any, items []patchItem) [][]byte {
	var groups [][]patchItem
	var group []patchItem
	size, urls := 0, 0
	for _, item := range items {
		n := len(item.cid) + len(item.value) + 4 // quotes, colon, comma
		if len(group) > 0 && (size+n > maxBody || urls+item.urls > maxURLs) {
			groups, group, size, urls = append(groups, group), nil, 0, 0
		}
		group, size, urls = append(group, item), size+n, urls+item.urls
	}
	if len(group) > 0 || len(settings) > 0 {
		groups = append(groups, group)
	}
	out := make([][]byte, 0, len(groups))
	for i, group := range groups {
		body := map[string]any{}
		if i == 0 {
			maps.Copy(body, settings)
		}
		if len(group) > 0 {
			hints := make(map[string]json.RawMessage, len(group))
			for _, item := range group {
				hints[item.cid] = item.value
			}
			body["hints"] = hints
		}
		out = append(out, mustJSON(body))
	}
	return out
}

// mustJSON encodes values whose encoding cannot fail.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
