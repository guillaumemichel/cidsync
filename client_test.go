package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
)

func TestClientGetUnknownSection(t *testing.T) {
	index := newFakeIndex()
	st, err := testClient(index).get(t.Context(), "files")
	if err != nil {
		t.Fatal(err)
	}
	if st.exists || len(st.Section.Hints) != 0 || !st.now.Equal(testNow) {
		t.Errorf("get = %+v, want an empty section as of the reply's Date", st)
	}
}

func TestClientGet(t *testing.T) {
	index := newFakeIndex()
	index.seed(t, "", `{"base":"https://tiles.example/"}`)
	index.seed(t, "files", `{"base":"files/","range":true,"hints":{"cidA":{"urls":["a.txt"],"size":5,"expires":"`+in(25)+`"}}}`)
	st, err := testClient(index).get(t.Context(), "files")
	if err != nil {
		t.Fatal(err)
	}
	a := st.Section.Hints["cidA"]
	if !st.exists || st.Section.Base != "files/" || len(a.URLs) != 1 || *a.Size != 5 || a.Expires.Format(timeFormat) != in(25) {
		t.Errorf("get = %+v", st)
	}
	if st.Effective.Base != "https://tiles.example/files/" || st.Effective.Range == nil || !*st.Effective.Range {
		t.Errorf("effective = %+v, want the resolved base and the range", st.Effective)
	}
}

func TestClientErrors(t *testing.T) {
	tests := []struct {
		name  string
		setup func(c *client)
		want  string
	}{
		{"wrong key", func(c *client) { c.key = "nope" }, "key"},
		{"wrong URL", func(c *client) { c.url += "/nowhere" }, "404"},
		{"no index", func(c *client) { c.url = "https://elsewhere.test" }, "connection refused"},
	}
	for _, tc := range tests {
		synctest.Test(t, func(t *testing.T) {
			c := testClient(newFakeIndex())
			tc.setup(c)
			if _, err := c.get(t.Context(), "files"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: get = %v, want an error mentioning %q", tc.name, err, tc.want)
			}
		})
	}
	c := testClient(newFakeIndex())
	c.key = "nope"
	if _, err := c.patch(t.Context(), "files", []byte(`{}`)); !errors.Is(err, errKey) {
		t.Errorf("patch with a wrong key = %v, want errKey", err)
	}
}

func TestClientPatch(t *testing.T) {
	index := newFakeIndex()
	c := testClient(index)
	offers, err := c.patch(t.Context(), "files", []byte(`{"base":"https://tiles.example/","hints":{"cidA":{"urls":["a.txt"],"size":5,"expires":"`+in(30)+`"}}}`))
	if err != nil || offers != 1 {
		t.Fatalf("patch = %d, %v, want 1 offer", offers, err)
	}
	_, err = c.patch(t.Context(), "files", []byte(`{"hints":{"cidB":{"expires":"`+in(30)+`"}}}`))
	if err == nil || !strings.Contains(err.Error(), "hints.cidB: urls: required") {
		t.Errorf("patch of a new CID without urls = %v, want the index's detail", err)
	}
	if got := len(index.patches("files")); got != 2 {
		t.Errorf("%d patches sent, want 2: a 400 is not retried", got)
	}
}

func TestClientRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		index := newFakeIndex()
		index.fail = 3
		offers, err := testClient(index).patch(t.Context(), "files", []byte(`{"base":"https://tiles.example/"}`))
		if err != nil || offers != 0 {
			t.Errorf("patch after three 503s = %d, %v, want it through", offers, err)
		}
		index.fail = 4
		if _, err := testClient(index).get(t.Context(), "files"); err == nil || !strings.Contains(err.Error(), "503") {
			t.Errorf("get after four 503s = %v, want the 503", err)
		}
		if got := len(index.requests); got != 8 {
			t.Errorf("%d requests, want 8: each tried four times", got)
		}
	})
}

func TestServerTime(t *testing.T) {
	res := &http.Response{Header: http.Header{"Date": {"Thu, 22 Oct 2026 12:00:00 GMT"}}}
	if got := serverTime(res); !got.Equal(testNow) {
		t.Errorf("serverTime = %v, want the Date", got)
	}
	if got := serverTime(&http.Response{Header: http.Header{}}); got.IsZero() || got.Nanosecond() != 0 {
		t.Errorf("serverTime without a Date = %v, want the local time to the second", got)
	}
}
