package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// watch starts a watch under ctx with index as index.test, and
// returns the channel its exit code comes on.
func (h *harness) watch(ctx context.Context, index http.Handler) <-chan int {
	e := h.newEnv(index)
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"watch", "-config", h.config, "-no-check"}, e) }()
	return done
}

// newWatchHarness is a harness for a watch, its fake index on the
// bubble's clock.
func newWatchHarness(t *testing.T) *harness {
	h := newHarness(t, filesSection)
	h.index.now = time.Time{}
	h.write("hello.txt", "hello\n")
	return h
}

// pass lets the watch run its next pass, a minute on.
func pass() {
	time.Sleep(time.Minute)
	synctest.Wait()
}

func TestWatchPublishesSettledFiles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newWatchHarness(t)
		ctx, cancel := context.WithCancel(t.Context())
		done := h.watch(ctx, h.index)
		synctest.Wait()
		if n := len(h.index.patches("files")); n != 0 {
			t.Errorf("first pass: %d patches, want none: a file is published once two scans agree", n)
		}
		pass()
		if h.hint("files", helloRaw) == "" {
			t.Errorf("second pass: hello.txt not published, %s", h.logs)
		}

		h.write("big.bin", "part")
		pass()
		h.write("big.bin", "partial")
		pass()
		big := sumCID(codecRaw, []byte("partial"))
		if h.hint("files", big) != "" || len(h.index.patches("files")) != 1 {
			t.Errorf("a file still growing was published: %v", h.index.patches("files"))
		}
		pass()
		if h.hint("files", big) == "" {
			t.Errorf("big.bin not published once it held still, %s", h.logs)
		}

		os.Remove(filepath.Join(h.dir, "hello.txt"))
		pass()
		if h.hint("files", helloRaw) != "" {
			t.Errorf("hello.txt still offered after its removal")
		}
		patches := len(h.index.patches("files"))
		pass()
		if n := len(h.index.patches("files")); n != patches {
			t.Errorf("a pass with nothing new sent %d patches", n-patches)
		}

		cancel()
		if code := <-done; code != 0 {
			t.Errorf("watch stopped with exit %d, want 0", code)
		}
	})
}

func TestWatchRenews(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newWatchHarness(t)
		ctx, cancel := context.WithCancel(t.Context())
		done := h.watch(ctx, h.index)
		synctest.Wait()
		pass()
		first := h.hint("files", helloRaw)
		time.Sleep(7*24*time.Hour - time.Minute)
		synctest.Wait()
		if got := h.hint("files", helloRaw); got != first {
			t.Errorf("renewed before 7 days: %s, was %s", got, first)
		}
		pass()
		pass()
		renewed := `"expires":"` + time.Now().Add(30*24*time.Hour).UTC().Format(timeFormat) + `"`
		if got := h.hint("files", helloRaw); !strings.Contains(got, renewed) {
			t.Errorf("after 7 days: %s, want %s", got, renewed)
		}
		cancel()
		<-done
	})
}

func TestWatchRetriesAfterFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newWatchHarness(t)
		h.index.refuse = map[string]string{"files": "base: not absolute"}
		ctx, cancel := context.WithCancel(t.Context())
		done := h.watch(ctx, h.index)
		synctest.Wait()
		pass()
		h.index.mu.Lock()
		h.index.refuse = nil
		h.index.mu.Unlock()
		pass()
		if h.hint("files", helloRaw) == "" {
			t.Errorf("not published on the pass after a refused one, %s", h.logs)
		}
		cancel()
		<-done
	})
}

// TestWatchStopsAfterRequest stops the watch from inside a patch: the
// patch completes, and watch exits 0.
func TestWatchStopsAfterRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newWatchHarness(t)
		ctx, cancel := context.WithCancel(t.Context())
		stopping := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPatch {
				cancel()
			}
			h.index.ServeHTTP(w, r)
		})
		done := h.watch(ctx, stopping)
		synctest.Wait()
		pass()
		if code := <-done; code != 0 {
			t.Errorf("exit %d, want 0", code)
		}
		if h.hint("files", helloRaw) == "" {
			t.Error("the patch in flight when the watch was stopped did not complete")
		}
	})
}
