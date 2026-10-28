package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A harness runs the command line against a fake index at
// index.test and a file host at files.test serving dir.
type harness struct {
	t      *testing.T
	dir    string
	config string
	index  *fakeIndex
	hosts  routes // beside index.test
	env    map[string]string
	logs   *logBuffer
	stdout bytes.Buffer
	stderr bytes.Buffer
}

// newHarness writes a config of sections, where DIR stands for the
// harness's directory.
func newHarness(t *testing.T, sections string) *harness {
	t.Helper()
	h := &harness{t: t, dir: t.TempDir(), index: newFakeIndex()}
	h.hosts = routes{"files.test": fileHost(h.dir)}
	h.env = map[string]string{"CIDSYNC_KEY": testKey, "CACHE_DIRECTORY": t.TempDir()}
	h.config = filepath.Join(t.TempDir(), "publish.json")
	doc := `{"index": "https://index.test", "sections": ` + strings.ReplaceAll(sections, "DIR", h.dir) + `}`
	writeFile(t, h.config, []byte(doc))
	return h
}

// fileHost serves dir as a static host does, readable cross-origin.
func fileHost(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		files.ServeHTTP(w, r)
	})
}

func (h *harness) run(args ...string) int {
	e := h.newEnv(h.index)
	code := run(h.t.Context(), args, e)
	h.assertKeyHidden(args)
	return code
}

// newEnv is a run's environment, with index as index.test.
func (h *harness) newEnv(index http.Handler) env {
	log, logs := newTestLogger()
	h.logs = logs
	h.stdout.Reset()
	h.stderr.Reset()
	hosts := routes{"index.test": index}
	maps.Copy(hosts, h.hosts)
	return env{
		getenv: func(k string) string { return h.env[k] },
		stdout: &h.stdout,
		stderr: &h.stderr,
		log:    log,
		http:   &http.Client{Transport: hosts},
	}
}

func (h *harness) assertKeyHidden(args []string) {
	if strings.Contains(h.logs.String()+h.stdout.String()+h.stderr.String(), testKey) {
		h.t.Errorf("%v: the key was printed", args)
	}
}

func (h *harness) sync(flags ...string) int {
	return h.run(append([]string{"sync", "-config", h.config}, flags...)...)
}

func (h *harness) write(name, data string) {
	writeFile(h.t, filepath.Join(h.dir, name), []byte(data))
}

// hint is what the fake holds for a CID of a section, as JSON.
func (h *harness) hint(section, cid string) string {
	hints, _ := h.index.section(section)["hints"].(map[string]any)
	if hints[cid] == nil {
		return ""
	}
	return string(mustJSON(hints[cid]))
}

// synced is the logged synced line of a section.
func (h *harness) synced(section string) map[string]any {
	for _, record := range h.logs.lines("synced") {
		if record["section"] == section {
			return record
		}
	}
	return nil
}

const filesSection = `{"files": {"dir": "DIR", "base": "https://files.test/", "range": true}}`

func TestSyncLifecycle(t *testing.T) {
	h := newHarness(t, filesSection)
	h.write("hello.txt", "hello\n")
	if code := h.sync("-no-check"); code != 0 {
		t.Fatalf("first sync: exit %d, %s", code, h.logs)
	}
	if got, want := h.hint("files", helloRaw), `{"expires":"`+in(30)+`","size":6,"urls":["hello.txt"]}`; got != want {
		t.Errorf("after the first sync: %s, want %s", got, want)
	}
	if s := h.synced("files"); s["added"] != 1.0 || s["offers"] != 1.0 {
		t.Errorf("synced line %v, want 1 added, 1 offer", s)
	}

	h.sync("-no-check")
	if n := len(h.index.patches("files")); n != 1 {
		t.Errorf("a second sync sent %d patches in all, want the first only", n)
	}

	h.index.now = testNow.Add(24 * time.Hour)
	h.write("b.txt", "b")
	h.sync("-no-check")
	patches := h.index.patches("files")
	if want := `{"hints":{"` + sumCID(codecRaw, []byte("b")) + `":{"urls":["b.txt"],"size":1,"expires":"` + in(30) + `"}}}`; patches[len(patches)-1] != want {
		t.Errorf("new file: patch %s, want %s: the new CID lapses with the rest", patches[len(patches)-1], want)
	}

	os.Rename(filepath.Join(h.dir, "hello.txt"), filepath.Join(h.dir, "hi.txt"))
	h.sync("-no-check")
	if got := h.hint("files", helloRaw); !strings.Contains(got, `"urls":["hi.txt"]`) {
		t.Errorf("after a rename: %s, want hi.txt", got)
	}

	os.Remove(filepath.Join(h.dir, "b.txt"))
	h.sync("-no-check")
	patches = h.index.patches("files")
	if want := `{"hints":{"` + sumCID(codecRaw, []byte("b")) + `":null}}`; patches[len(patches)-1] != want {
		t.Errorf("removal: patch %s, want %s", patches[len(patches)-1], want)
	}
}

func TestSyncSettingsOnly(t *testing.T) {
	h := newHarness(t, filesSection)
	h.write("hello.txt", "hello\n")
	h.index.seed(t, "files", `{"base":"https://files.test/","hints":{"`+helloRaw+`":{"urls":["hello.txt"],"size":6,"expires":"`+in(25)+`"}}}`)
	if code := h.sync("-no-check"); code != 0 {
		t.Fatalf("exit %d, %s", code, h.logs)
	}
	if got := h.index.patches("files"); len(got) != 1 || got[0] != `{"range":true}` {
		t.Errorf("patches %q, want the range alone", got)
	}
}

func TestSyncDryRun(t *testing.T) {
	h := newHarness(t, filesSection)
	h.write("hello.txt", "hello\n")
	if code := h.sync("-no-check", "-dry-run"); code != 0 {
		t.Fatalf("exit %d, %s", code, h.logs)
	}
	if n := len(h.index.patches("files")); n != 0 {
		t.Errorf("dry run sent %d patches", n)
	}
	var printed struct {
		Method, Path string
		Body         json.RawMessage
	}
	if err := json.Unmarshal(h.stdout.Bytes(), &printed); err != nil || printed.Method != "PATCH" || printed.Path != "/sections/files" || !bytes.Contains(printed.Body, []byte(helloRaw)) {
		t.Errorf("printed %q, %v, want the patch as a JSON line", h.stdout.String(), err)
	}
}

func TestSyncHashesOnce(t *testing.T) {
	h := newHarness(t, `{"files": {"dir": "DIR", "base": "https://files.test/"}, "mirror": {"dir": "DIR", "base": "https://mirror.test/"}}`)
	h.write("hello.txt", "hello\n")
	if code := h.sync("-no-check"); code != 0 {
		t.Fatalf("exit %d, %s", code, h.logs)
	}
	if n := len(h.logs.lines("hashed")); n != 1 {
		t.Errorf("hashed %d times, want once for both sections", n)
	}
	h.sync("-no-check")
	if n := len(h.logs.lines("hashed")); n != 0 {
		t.Errorf("hashed %d times in a second run, want none: the cache knows the file", n)
	}
}

func TestSyncGuard(t *testing.T) {
	h := newHarness(t, filesSection)
	h.write("a.txt", "a")
	h.write("b.txt", "b")
	h.sync("-no-check")
	os.Remove(filepath.Join(h.dir, "a.txt"))
	os.Remove(filepath.Join(h.dir, "b.txt"))
	if code := h.sync("-no-check"); code != 1 || len(h.index.patches("files")) != 1 {
		t.Errorf("emptied directory: exit %d, %d patches, want 1 and only the first", code, len(h.index.patches("files")))
	}
	if code := h.sync("-no-check", "-force"); code != 0 || len(h.index.patches("files")) != 2 {
		t.Errorf("with -force: exit %d, %d patches, want 0 and the removals sent", code, len(h.index.patches("files")))
	}
}

func TestSyncFailures(t *testing.T) {
	h := newHarness(t, `{"a": {"dir": "DIR", "base": "https://files.test/"}, "b": {"dir": "DIR", "base": "https://files.test/"}}`)
	h.write("hello.txt", "hello\n")
	h.index.refuse = map[string]string{"a": "base: not absolute"}
	if code := h.sync("-no-check"); code != 1 {
		t.Errorf("one section refused: exit %d, want 1", code)
	}
	if h.synced("b") == nil {
		t.Errorf("section b not synced after a failed: %s", h.logs)
	}
	h.env["CIDSYNC_KEY"] = "wrong"
	if code := h.sync("-no-check"); code != 2 || len(h.logs.lines("sync failed")) != 1 {
		t.Errorf("wrong key: exit %d, %s, want 2 at the first section", code, h.logs)
	}
	delete(h.env, "CIDSYNC_KEY")
	if code := h.sync("-no-check"); code != 2 || !strings.Contains(h.stderr.String()+h.logs.String(), "CIDSYNC_KEY") {
		t.Errorf("no key: exit %d, want 2 and a word on where the key comes from", code)
	}
}

func TestSyncKeyFile(t *testing.T) {
	h := newHarness(t, filesSection)
	keyFile := filepath.Join(t.TempDir(), "key")
	writeFile(t, keyFile, []byte(testKey+"\n"))
	h.env["CIDSYNC_KEY"] = "wrong"
	if code := h.sync("-no-check", "-key-file", keyFile); code != 0 {
		t.Errorf("exit %d with the key in a file, %s", code, h.logs)
	}
}

func TestRenew(t *testing.T) {
	h := newHarness(t, `{"files": {"dir": "DIR", "base": "https://files.test/"}, "idle": {"dir": "DIR", "base": "https://files.test/"}, "fresh": {"dir": "DIR", "base": "https://files.test/"}}`)
	h.index.seed(t, "files", `{"base":"https://files.test/","hints":{"cidA":{"urls":["a.txt"],"size":5,"expires":"`+in(22)+`","range":false},"cidB":{"urls":["b.txt"],"expires":"`+in(29)+`"}}}`)
	h.index.seed(t, "fresh", `{"base":"https://files.test/","hints":{"cidA":{"urls":["a.txt"],"size":5,"expires":"`+in(25)+`"}}}`)
	if code := h.run("renew", "-config", h.config); code != 0 {
		t.Fatalf("exit %d, %s", code, h.logs)
	}
	want := `{"hints":{"cidA":{"urls":["a.txt"],"size":5,"expires":"` + in(30) + `"},"cidB":{"urls":["b.txt"],"expires":"` + in(30) + `"}}}`
	if got := h.index.patches("files"); len(got) != 1 || got[0] != want {
		t.Errorf("renewal due: patches %q, want %s", got, want)
	}
	if got := len(h.index.patches("fresh")) + len(h.index.patches("idle")); got != 0 {
		t.Errorf("%d patches to sections not due or empty, want none", got)
	}
	if code := h.run("renew", "-config", h.config, "-section", "fresh"); code != 0 || len(h.index.patches("files")) != 1 {
		t.Errorf("-section fresh: exit %d, want 0 and nothing more sent to files", code)
	}
	if code := h.run("renew", "-config", h.config, "-section", "nope"); code != 2 {
		t.Errorf("-section of no section: exit %d, want 2", code)
	}
}
