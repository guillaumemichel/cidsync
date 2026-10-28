# cidsync

A mirror tells the index which bytes it serves where: by CID, in
sections, each with a base URL and settings of its own. cidsync keeps
those sections in sync with the directories behind the base URLs. It scans
each directory into the CIDs its files have, compares them with what the
index holds, and sends the difference. Files added, changed, renamed or
removed since the last run all show up, whoever made the change. Every CID
of a section shares one expiry, which the tool renews before it lapses.

The index is `index.content-router.com`, a [FISL](https://dasl.ing/fisl.html)
index: a page looks a CID up there, gets the URLs of its copies, and checks
every byte against the CID. Writing to it takes a key from its operator.

The directory is the truth, the index the copy. The only local state is a
cache of file hashes, which can be deleted at any time.

## Install

```
go install github.com/guillaumemichel/cidsync@latest
```

Go 1.26 or later, standard library only. `CGO_ENABLED=0 go build` makes a
static binary.

## Use

```
cidsync sync  -config FILE [-key-file FILE] [-dry-run] [-force] [-no-check]
cidsync renew -config FILE [-key-file FILE] [-section NAME]
cidsync watch -config FILE [-key-file FILE] [-interval 1m] [-no-check]
cidsync cid   FILE...
```

- `sync` brings every section of the config in line with its directory.
- `renew` renews what the index holds once a renewal is due, without
  reading any directory: for a job that does not have the files, such as a
  scheduled CI run. It cannot bring back a CID that lapsed; `sync` can.
- `watch` scans every interval and syncs what changed ([Watch](#watch)).
- `cid` prints the CIDs a scan would publish of each file.

| flag | meaning |
| --- | --- |
| `-key-file` | a file holding the key, in place of `CIDSYNC_KEY` |
| `-dry-run` | print each patch as a JSON line on stdout instead of sending it |
| `-force` | let a sync leave a section with fewer than half of its CIDs |
| `-no-check` | publish URLs without reading a byte of each first |
| `-section` | renew this section only; `""` is the catalog |
| `-interval` | how often `watch` scans |

The key comes from `CIDSYNC_KEY` or `-key-file`; systemd's `LoadCredential`
gives such a file. The key is never logged or printed.

## Config

```json
{
  "index": "https://index.content-router.com",
  "sections": {
    "tiles": {
      "dir": "/srv/tiles",
      "base": "https://tiles.acme.example/",
      "range": true,
      "exclude": ["*.tmp", "drafts/*"]
    }
  }
}
```

| field | default | meaning |
| --- | --- | --- |
| `index` | required | the index's URL; `https`, or plain `http` to a loopback host |
| `sections` | required | section name → section; the name `""` is the catalog, the root of your sections |
| `dir` | required | the directory served at the section's base; relative to the config file |
| `base` | – | the section's base: an absolute `https` URL ending in `/`, or a path below its parent's base, such as `eu/`; the catalog needs an absolute one |
| `parent` | the catalog | the section's parent section |
| `range` | – | `true` when every URL of the section answers a byte-range request with a `206` |
| `lifetime` | `30d` | how long the section's CIDs live after a renewal; at most `365d` |
| `renew` | `7d` | how long after a renewal the next is due; less than `lifetime` |
| `exclude` | `[]` | glob patterns of paths to leave out ([What a directory publishes](#what-a-directory-publishes)) |

`base`, `parent` and `range` are the section's settings: a sync sets each
as the config says, and removes one the config leaves out, which the
section then inherits. Durations take Go's syntax plus `d` for days, as in
`1d12h`. Unknown fields are an error: the file is written by hand, and a
typo must not pass silently.

The tool owns the sections in its config: a CID it did not put there is
removed. One publisher per section.

## What a directory publishes

The scan walks `dir`. A file's path below `dir` is its URL below `base`.

- **Left out**: a name starting with `.`, and everything below such a
  directory: rsync's temporary files, editors' swap files, `.git`. A path
  matching an `exclude` pattern: a pattern with a `/` matches the whole
  path, one without matches a name at any depth (`*.tmp`). A symlinked
  file is followed, a symlinked directory is not, a broken link is left
  out with a warning.
- **A packed file** `X`, with `X.proofs/root` beside it
  ([veritiles](https://github.com/guillaumemichel/veritiles)), is
  published as two CIDs, and `X` is never read: the anchor, the CID of the
  descriptor `X.proofs/root`, at that path; and the map CID the descriptor
  names, at `X`, with the size the descriptor gives. A descriptor veritiles
  would refuse is left out with a warning. While `X` is not the size its
  descriptor gives, or `X.proofs/` has no `root`, a copy or a packing is
  under way, and both wait. The shards are not published: clients find
  them beside `root`.
- **Any other file** is published as its raw sha2-256 CID, the CID
  veritiles reads a raw asset by.

A CID found at several paths gets them all, sorted, up to 16: the index
keeps 16. Each path segment is percent-encoded, `:` included, so that a
first segment such as `a:b` cannot read as a scheme. A path over 256 bytes
once encoded is left out with a warning, as the index would refuse it.

`cidsync cid FILE` prints what the scan makes of a file.

The hash cache is a JSON file in systemd's `$CACHE_DIRECTORY` when there is
one, else in the user's cache directory (`~/.cache/cidsync/` on Linux). A
file whose size and modification time have not changed is not read again.

## Sync

For each section, `sync` reads what the index holds (`GET`; a section it
does not know is empty, and the first patch creates it), checks the URLs
it is about to add, and sends what differs as `PATCH` requests:

| the CID is | sent |
| --- | --- |
| in the directory, not in the index | `{"urls": […], "size": n, "expires": E}` |
| in the index, not in the directory | `null` |
| in both, with other paths or another size | `{"urls": […], "size": n, "expires": E}` |
| in both, the same | nothing, unless a renewal is due |

An entry is always an object, never a bare URL list, which would also drop
the CID's own settings and give it 30 days. A CID that carries a range of
its own, set by hand, has it removed. A settings change is sent alone when
nothing else changes.

### Expiry

Every CID of a section expires at the same time, `E`, so that the section
lapses as one. `E` is the earliest expiry the index holds for the section,
so a new CID lapses with the rest. A renewal is due `renew` after the last
one, that is once `E` is less than `lifetime − renew` away; a sync then
sends every CID with `E = now + lifetime`, its paths and size included, so
that a CID that lapsed meanwhile comes back. `now` is the index's clock,
read from the `Date` of its reply. With the defaults, a sync at least once
a week keeps a section alive with 23 days to spare.

### Guard

An unmounted or emptied directory would withdraw every offer. A sync that
would leave a section with fewer than half of its CIDs is refused unless
`-force`; CIDs replaced by others, as when files change, do not count. A
missing `dir` is an error, never an empty section.

### Check

Before a URL is published, one `GET` of its first byte, with an `Origin`,
checks it serves. A URL the index already holds for its CID is not read
again.

- The URL must answer `206`, or `200` where the section does not declare
  `range`.
- An answer without `Access-Control-Allow-Origin` is logged: web pages
  cannot read it.

A CID with a URL that fails stays as the index holds it, or out when it
holds none, and the run exits 1. Eight checks run at once.

### Writes

Each patch is at most 7 MiB and 65,536 URLs, within the index's limits;
the first carries the settings and the removals, so that the offers they
free count before the next patches add theirs. Each patch applies whole,
and a failure stops that section: the next sync picks up where it stopped.
Every body is deterministic and every value absolute, so a patch sent
twice changes nothing: a transport error or a `5xx` is retried after 1, 4
and 16 seconds. A `4xx` is not retried, and a `400` logs the index's
`detail`.

## Watch

`watch` scans every section each interval. A new or changed file is
published once two scans in a row find it unchanged, so a file still being
copied waits; the first scan after a start publishes nothing. A section is
written when its files change, when its renewal falls due, and every 6
hours in any case, with what `sync` would send. A section whose sync fails,
or whose checks fail, is synced again on the next scan. `SIGTERM` or
`SIGINT` stops it once the section in flight is done.

## Logs and exit codes

Logs are JSON lines on stderr, one per section synced:

```
{"time":"…","level":"INFO","msg":"synced","section":"tiles","added":1,"changed":0,"removed":0,"renewed":false,"offers":6}
```

`skipped` names a file left out and why, `check failed` a URL that failed
its check, and `sync failed` a section that failed.

| exit | when |
| --- | --- |
| 0 | every section synced |
| 1 | a section failed, or a check did; the other sections ran |
| 2 | usage, config or key error: running again would repeat it |

## Deploy

**On the host that serves the files**: run `cidsync sync` daily, from cron
or a systemd timer, and again after a copy to publish it at once. Or run
`cidsync watch` as a long-running service, which publishes a copy within
two intervals.

Copy files so that none is published half done: rsync writes to a
temporary dotfile and renames it into place (pass `--partial-dir=.rsync-partial`
when resuming, not `--partial`, which leaves a partial file under its
final name). Copy a packed file's `X.proofs/` before `X`: the pair is
published once `X` has the size its descriptor gives, and by then every
shard is in place.

**From GitHub Actions**: deploy, then sync, so that a URL is live before
it is offered; renew on a schedule:

```yaml
on:
  push: { branches: [main] }
  schedule: [{ cron: "0 4 * * *" }]
jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - run: make site && ./deploy.sh public
        if: github.event_name == 'push'
      - run: go install github.com/guillaumemichel/cidsync@latest
      - run: ~/go/bin/cidsync ${{ github.event_name == 'push' && 'sync' || 'renew' }} -config cidsync.json
        env: { CIDSYNC_KEY: "${{ secrets.CIDSYNC_KEY }}" }
```

GitHub disables the scheduled workflows of a public repository after 60
days without activity, and with the default lifetime the sections then
lapse 30 days later: a project that may go quiet should set `lifetime` up
to `365d`, or renew from elsewhere.

## License

[MIT](LICENSE)
