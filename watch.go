package main

import (
	"context"
	"errors"
	"slices"
	"time"
)

// A section is synced at least this often, in case it was changed
// elsewhere or a CID lapsed; a sync that finds nothing to change sends
// nothing.
const refreshEvery = 6 * time.Hour

// A watcher remembers, for each section, the files of its last scan and
// what its last sync published.
type watcher struct {
	p      *publisher
	scans  map[string][]found
	synced map[string]watchState
}

type watchState struct {
	files   []found   // the settled files it published
	expires time.Time // the expiry it gave
	at      time.Time
}

// watch syncs the config's sections every interval until ctx is done. A
// file is published once two scans in a row find it unchanged, so that one
// still being copied waits. A section is written when its settled files
// change, when its renewal falls due, and every 6 hours. A stop lets the
// section in flight finish.
func (p *publisher) watch(ctx context.Context, interval time.Duration) int {
	w := &watcher{p: p, scans: map[string][]found{}, synced: map[string]watchState{}}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := w.pass(ctx); errors.Is(err, errKey) {
			return exitConfig
		}
		select {
		case <-ctx.Done():
			return exitOK
		case <-ticker.C:
		}
	}
}

// pass scans every section, and syncs those that need it.
func (w *watcher) pass(ctx context.Context) error {
	defer w.p.saveCache()
	for _, name := range sectionNames(w.p.cfg) {
		if ctx.Err() != nil {
			return nil
		}
		err := w.passSection(context.WithoutCancel(ctx), name)
		if err != nil {
			w.p.log.Error("sync failed", "section", name, "err", err)
		}
		if errors.Is(err, errKey) {
			return err
		}
	}
	return nil
}

// passSection syncs a section when it needs it. Until a sync succeeds,
// with every URL checked, the next pass tries again.
func (w *watcher) passSection(ctx context.Context, name string) error {
	sec := w.p.cfg.Sections[name]
	log := w.p.log.With("section", name)
	found, err := walk(sec.Dir, sec.Exclude, log)
	if err != nil {
		return err
	}
	prev, scanned := w.scans[name]
	w.scans[name] = found
	if !scanned {
		return nil
	}
	settled := settle(found, prev)
	last, synced := w.synced[name]
	if synced && slices.EqualFunc(last.files, settled, sameFile) && !due(sec, last) {
		return nil
	}
	delete(w.synced, name)
	files, err := identify(settled, w.p.cache, log)
	if err != nil {
		return err
	}
	out, err := w.p.publish(ctx, name, entries(files, log), log)
	if err != nil || out.unchecked > 0 {
		return err
	}
	w.synced[name] = watchState{files: settled, expires: out.expires, at: time.Now()}
	return nil
}

// due says whether a section must be synced though its files did not
// change: its renewal is due, or its last sync is 6 hours old.
func due(sec *Section, last watchState) bool {
	now := time.Now()
	return last.expires.Sub(now) < sec.lifetime-sec.renew || now.Sub(last.at) >= refreshEvery
}

// settle keeps the files a scan found as the scan before found them: one
// that changed meanwhile is still being written, and waits.
func settle(scan, prev []found) []found {
	before := make(map[string]found, len(prev))
	for _, f := range prev {
		before[f.rel] = f
	}
	var out []found
	for _, f := range scan {
		if b, ok := before[f.rel]; ok && sameFile(b, f) {
			out = append(out, f)
		}
	}
	return out
}

func sameFile(a, b found) bool {
	return a.rel == b.rel && a.at == b.at && equal(a.root, b.root)
}
