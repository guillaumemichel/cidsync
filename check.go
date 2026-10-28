package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"
)

const (
	maxChecks    = 8
	checkTimeout = 30 * time.Second
	// Any page's origin: the check asks whether a page may read the answer.
	checkOrigin = "https://cidsync.invalid"
)

type checkJob struct {
	cid, path string
	size      int64
}

// checked reads one byte of each URL a section is about to offer that the
// index does not offer yet. A CID with a URL that fails is kept as the
// index holds it, or left out when it holds none; checked returns how
// many.
func (p *publisher) checked(ctx context.Context, name string, scan map[string]entry, st *stored, log *slog.Logger) (map[string]entry, int, error) {
	jobs := newPaths(scan, st)
	if len(jobs) == 0 {
		return scan, 0, nil
	}
	sec := p.cfg.Sections[name]
	target, err := p.target(ctx, name, sec, st)
	if err != nil {
		return nil, 0, fmt.Errorf("check: %w", err)
	}
	base, err := url.Parse(target.Base)
	if err != nil || !base.IsAbs() {
		return nil, 0, fmt.Errorf("check: no base to read the URLs at: %q", target.Base)
	}
	ranged := target.Range != nil && *target.Range
	failed := p.checkAll(ctx, base, jobs, ranged, log)
	out := maps.Clone(scan)
	for cid := range failed {
		if old, ok := st.Section.Hints[cid]; ok {
			out[cid] = entry{urls: old.URLs, size: scan[cid].size}
		} else {
			delete(out, cid)
		}
	}
	return out, len(failed), nil
}

// newPaths lists the paths of a scan that the index does not hold for
// their CID.
func newPaths(scan map[string]entry, st *stored) []checkJob {
	var jobs []checkJob
	for cid, e := range scan {
		held := st.Section.Hints[cid].URLs
		for _, path := range e.urls {
			if !slices.Contains(held, path) {
				jobs = append(jobs, checkJob{cid: cid, path: path, size: e.size})
			}
		}
	}
	return jobs
}

// target is what a section's CIDs get once the sync writes its settings:
// what they get now when the settings stay, else the config's own over
// what the parent passes down.
func (p *publisher) target(ctx context.Context, name string, sec *Section, st *stored) (settings, error) {
	if st.exists && len(settingsDiff(sec, st.Section)) == 0 {
		return st.Effective, nil
	}
	var inherited settings
	if name != "" {
		parent, err := p.client.get(ctx, sec.Parent)
		if err != nil {
			return settings{}, err
		}
		inherited = parent.Effective
	}
	return inherit(sec, inherited)
}

func inherit(sec *Section, parent settings) (settings, error) {
	out := parent
	if sec.Range != nil {
		out.Range = sec.Range
	}
	if sec.Base == "" {
		return out, nil
	}
	ref, err := url.Parse(sec.Base)
	if err != nil || ref.IsAbs() {
		out.Base = sec.Base
		return out, err
	}
	top, err := url.Parse(parent.Base)
	if err != nil || parent.Base == "" {
		return settings{}, fmt.Errorf("base %q: no parent base to resolve it against", sec.Base)
	}
	out.Base = top.ResolveReference(ref).String()
	return out, nil
}

// checkAll runs the checks on maxChecks workers, and returns the CIDs
// with a URL that failed.
func (p *publisher) checkAll(ctx context.Context, base *url.URL, jobs []checkJob, ranged bool, log *slog.Logger) map[string]bool {
	var mu sync.Mutex
	failed := map[string]bool{}
	queue := make(chan checkJob)
	var wg sync.WaitGroup
	for range maxChecks {
		wg.Go(func() {
			for job := range queue {
				if !p.checkOne(ctx, base, job, ranged, log) {
					mu.Lock()
					failed[job.cid] = true
					mu.Unlock()
				}
			}
		})
	}
	for _, job := range jobs {
		queue <- job
	}
	close(queue)
	wg.Wait()
	return failed
}

// checkOne checks a job's URL, and logs what it found.
func (p *publisher) checkOne(ctx context.Context, base *url.URL, job checkJob, ranged bool, log *slog.Logger) bool {
	ref, err := url.Parse(job.path)
	if err != nil {
		log.Error("check failed", "path", job.path, "err", err)
		return false
	}
	u := base.ResolveReference(ref).String()
	warning, err := checkURL(ctx, p.http, u, job.size, ranged)
	if warning != "" {
		log.Warn("check warning", "url", u, "warning", warning)
	}
	if err != nil {
		log.Error("check failed", "url", u, "err", err)
	}
	return err == nil
}

// checkURL reads the first byte of u as a page would. The host must
// answer 206, or 200 where the section declares no ranges.
func checkURL(ctx context.Context, hc *http.Client, u string, size int64, ranged bool) (warning string, err error) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Origin", checkOrigin)
	if size > 0 {
		req.Header.Set("Range", "bytes=0-0")
	}
	res, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	res.Body.Close() // unread: a 200 may be the whole file
	switch {
	case res.StatusCode == http.StatusPartialContent:
	case res.StatusCode == http.StatusOK && !(ranged && size > 0):
	case res.StatusCode == http.StatusOK:
		return "", errors.New("200, but the section declares range: want a 206")
	default:
		return "", errors.New(res.Status)
	}
	if res.Header.Get("Access-Control-Allow-Origin") == "" {
		return "no Access-Control-Allow-Origin: pages on other sites cannot read it", nil
	}
	return "", nil
}
