package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func runQuiet(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	log, _ := newTestLogger()
	var stdout, stderr bytes.Buffer
	e := env{getenv: func(string) string { return "" }, stdout: &stdout, stderr: &stderr, log: log}
	code := run(t.Context(), args, e)
	return code, stdout.String(), stderr.String()
}

func TestCID(t *testing.T) {
	hello := filepath.Join(t.TempDir(), "hello.txt")
	writeFile(t, hello, []byte("hello\n"))
	code, out, _ := runQuiet(t, "cid", hello, numbersRoot)
	want := helloRaw + "  " + hello + "\n" +
		numbersAnchor + "  " + numbersRoot + "\n" +
		numbersMap + "  testdata/numbers.txt\n"
	if code != 0 || out != want {
		t.Errorf("cid: exit %d, printed\n%s\nwant\n%s", code, out, want)
	}
	if code, _, stderr := runQuiet(t, "cid", filepath.Join(t.TempDir(), "gone")); code != 1 || !strings.Contains(stderr, "gone") {
		t.Errorf("cid of a missing file: exit %d, %q, want 1 and the error", code, stderr)
	}
}

func TestUsage(t *testing.T) {
	tests := []struct {
		args []string
		code int
		want string
	}{
		{nil, 2, "usage"},
		{[]string{"publish"}, 2, `unknown command "publish"`},
		{[]string{"cid"}, 2, "usage"},
		{[]string{"sync"}, 2, "-config is required"},
		{[]string{"sync", "-config", "x", "extra"}, 2, `unexpected argument "extra"`},
		{[]string{"watch", "-config", "x", "-interval", "0s"}, 2, "-interval"},
		{[]string{"renew", "-dry-run"}, 2, "dry-run"},
		{[]string{"sync", "-h"}, 0, "-no-check"},
	}
	for _, tc := range tests {
		code, _, stderr := runQuiet(t, tc.args...)
		if code != tc.code || !strings.Contains(stderr, tc.want) {
			t.Errorf("%q: exit %d, %q, want %d and %q", tc.args, code, stderr, tc.code, tc.want)
		}
	}
}

func TestLoadKey(t *testing.T) {
	getenv := func(k string) string { return map[string]string{"CIDSYNC_KEY": testKey}[k] }
	if key, err := loadKey("", getenv); key != testKey || err != nil {
		t.Errorf("loadKey from the environment = %v", err)
	}
	spaced := filepath.Join(t.TempDir(), "key")
	writeFile(t, spaced, []byte("a key"))
	if _, err := loadKey(spaced, getenv); err == nil || strings.Contains(err.Error(), "a key") {
		t.Errorf("a key with a space: %v, want an error that does not show it", err)
	}
	if _, err := loadKey(filepath.Join(t.TempDir(), "none"), getenv); err == nil {
		t.Error("a missing key file: no error")
	}
}
