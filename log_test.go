package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// A logBuffer collects JSON log lines; checks log from several goroutines.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lines returns the logged records whose msg is msg.
func (b *logBuffer) lines(msg string) []map[string]any {
	var out []map[string]any
	for line := range strings.Lines(b.String()) {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == msg {
			out = append(out, record)
		}
	}
	return out
}

func newTestLogger() (*slog.Logger, *logBuffer) {
	b := &logBuffer{}
	return slog.New(slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})), b
}

// skippedPaths lists the paths the scan logged as skipped.
func skippedPaths(t *testing.T, logs *logBuffer) []string {
	t.Helper()
	var out []string
	for _, record := range logs.lines("skipped") {
		out = append(out, record["path"].(string))
	}
	return out
}
