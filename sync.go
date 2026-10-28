package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"time"
)

// A publisher keeps a config's sections in sync with their directories.
type publisher struct {
	cfg    *Config
	client *client
	http   *http.Client // for the checks
	cache  *hashCache
	log    *slog.Logger
	out    io.Writer // where a dry run prints its patches

	dryRun, force, check bool
}

// An outcome is what a section's sync did.
type outcome struct {
	expires   time.Time // given to every CID it sent
	unchecked int       // CIDs the check kept back
}

// syncAll syncs every section of the config, and returns the exit code: a
// section that fails leaves the others to run, a key the index refuses
// stops them all.
func (p *publisher) syncAll(ctx context.Context) int {
	defer p.saveCache()
	code := exitOK
	for _, name := range sectionNames(p.cfg) {
		out, err := p.syncSection(ctx, name)
		if err != nil {
			p.log.Error("sync failed", "section", name, "err", err)
		}
		if errors.Is(err, errKey) {
			return exitConfig
		}
		if err != nil || out.unchecked > 0 {
			code = exitFailure
		}
	}
	return code
}

func (p *publisher) syncSection(ctx context.Context, name string) (outcome, error) {
	log := p.log.With("section", name)
	scan, err := scanSection(p.cfg.Sections[name], p.cache, log)
	if err != nil {
		return outcome{}, err
	}
	return p.publish(ctx, name, scan, log)
}

// publish brings a section to what a scan found: it reads what the index
// holds, checks the URLs it does not hold yet, and writes the difference.
func (p *publisher) publish(ctx context.Context, name string, scan map[string]entry, log *slog.Logger) (outcome, error) {
	sec := p.cfg.Sections[name]
	st, err := p.client.get(ctx, name)
	if err != nil {
		return outcome{}, err
	}
	var out outcome
	if p.check {
		if scan, out.unchecked, err = p.checked(ctx, name, scan, st, log); err != nil {
			return outcome{}, err
		}
	}
	c := diff(sec, scan, st)
	if err := guard(c, st, p.force); err != nil {
		return outcome{}, err
	}
	offers, err := p.write(ctx, name, c.bodies(st), st.Offers)
	if err != nil {
		return outcome{}, err
	}
	attrs := []any{"added", c.added, "changed", c.changed, "removed", len(c.removed), "renewed", c.renewed, "offers", offers}
	if out.unchecked > 0 {
		attrs = append(attrs, "unchecked", out.unchecked)
	}
	if p.dryRun {
		attrs = append(attrs, "dry_run", true)
	}
	log.Info("synced", attrs...)
	out.expires = c.expires
	return out, nil
}

// write sends a section's patches in order, or prints them on a dry run,
// and returns the account's live offers after the last.
func (p *publisher) write(ctx context.Context, name string, bodies [][]byte, offers int) (int, error) {
	for _, body := range bodies {
		if p.dryRun {
			fmt.Fprintf(p.out, "%s\n", mustJSON(map[string]any{"method": http.MethodPatch, "path": sectionPath(name), "body": json.RawMessage(body)}))
			continue
		}
		n, err := p.client.patch(ctx, name, body)
		if err != nil {
			return 0, err
		}
		offers = n
	}
	return offers, nil
}

func (p *publisher) saveCache() {
	var dirs []string
	for _, sec := range p.cfg.Sections {
		dirs = append(dirs, sec.Dir)
	}
	if err := p.cache.save(dirs); err != nil {
		p.log.Warn("hash cache not saved", "err", err)
	}
}

// renewAll renews the config's sections, or the one named.
func (p *publisher) renewAll(ctx context.Context, only *string) int {
	names := sectionNames(p.cfg)
	if only != nil {
		names = []string{*only}
	}
	code := exitOK
	for _, name := range names {
		err := p.renewSection(ctx, name)
		if err != nil {
			p.log.Error("renew failed", "section", name, "err", err)
			code = exitFailure
		}
		if errors.Is(err, errKey) {
			return exitConfig
		}
	}
	return code
}

// renewSection renews what the index holds of a section once a renewal is
// due, without reading the directory: for a job that does not have the
// files. A CID that lapsed is gone from the index, and only a sync can
// bring it back.
func (p *publisher) renewSection(ctx context.Context, name string) error {
	log := p.log.With("section", name)
	st, err := p.client.get(ctx, name)
	if err != nil {
		return err
	}
	expires, due := expiry(p.cfg.Sections[name], st)
	if !due {
		log.Info("renewal not due", "cids", len(st.Section.Hints))
		return nil
	}
	offers, err := p.write(ctx, name, batches(nil, renewal(st, expires)), st.Offers)
	if err != nil {
		return err
	}
	log.Info("renewed", "cids", len(st.Section.Hints), "expires", expires.Format(timeFormat), "offers", offers)
	return nil
}

// renewal sends every stored CID again, as the index holds it, with a
// new expiry. Its URLs and size go too: a CID that lapses between the GET
// and the patch is then sent again, not refused.
func renewal(st *stored, expires time.Time) []patchItem {
	var items []patchItem
	for _, cid := range slices.Sorted(maps.Keys(st.Section.Hints)) {
		e := st.Section.Hints[cid]
		h := hintPatch{URLs: e.URLs, Size: e.Size, Expires: expires.UTC().Format(timeFormat)}
		items = append(items, patchItem{cid: cid, value: mustJSON(h), urls: len(e.URLs)})
	}
	return items
}
