// cidsync keeps an index's sections in sync with directories of files: it
// scans each directory into the CIDs it holds, compares them with what the
// index offers, and sends the difference.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	exitOK      = 0
	exitFailure = 1 // a section failed; the others ran
	exitConfig  = 2 // usage, config or key: running again would only repeat it
)

const usage = `usage:
  cidsync sync  -config FILE [-key-file FILE] [-dry-run] [-force] [-no-check]
  cidsync renew -config FILE [-key-file FILE] [-section NAME]
  cidsync watch -config FILE [-key-file FILE] [-interval 1m] [-no-check]
  cidsync cid   FILE...

The key is CIDSYNC_KEY, or the content of -key-file.`

// An env is what a run takes from its process; tests give their own.
type env struct {
	getenv func(string) string
	stdout io.Writer
	stderr io.Writer
	log    *slog.Logger
	http   *http.Client
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop() // a second signal kills, as without the handler
	}()
	os.Exit(run(ctx, os.Args[1:], env{
		getenv: os.Getenv,
		stdout: os.Stdout,
		stderr: os.Stderr,
		log:    slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		http:   &http.Client{Timeout: requestTimeout},
	}))
}

func run(ctx context.Context, args []string, e env) int {
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, usage)
		return exitConfig
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "cid":
		return runCID(rest, e)
	case "sync", "renew", "watch":
	default:
		fmt.Fprintf(e.stderr, "unknown command %q\n%s\n", cmd, usage)
		return exitConfig
	}
	o, err := parseOptions(cmd, rest, e.stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n%s\n", err, usage)
		return exitConfig
	}
	p, err := newPublisher(o, e)
	if err != nil {
		e.log.Error("not started", "err", err)
		return exitConfig
	}
	switch cmd {
	case "renew":
		return p.renewAll(ctx, o.section)
	case "watch":
		return p.watch(ctx, o.interval)
	}
	return p.syncAll(ctx)
}

type options struct {
	config, keyFile      string
	section              *string // renew's -section: "" is the catalog
	dryRun, force, check bool
	interval             time.Duration
}

func parseOptions(cmd string, args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	var noCheck bool
	fs.StringVar(&o.config, "config", "", "the config `file`, required")
	fs.StringVar(&o.keyFile, "key-file", "", "a `file` holding the key, in place of CIDSYNC_KEY")
	switch cmd {
	case "sync":
		fs.BoolVar(&o.dryRun, "dry-run", false, "print the patches instead of sending them")
		fs.BoolVar(&o.force, "force", false, "remove more than half of a section's CIDs")
		fs.BoolVar(&noCheck, "no-check", false, "publish URLs without reading a byte of each first")
	case "renew":
		fs.Func("section", "renew the section `name` only; \"\" is the catalog", func(s string) error {
			o.section = &s
			return nil
		})
	case "watch":
		fs.DurationVar(&o.interval, "interval", time.Minute, "how often to scan")
		fs.BoolVar(&noCheck, "no-check", false, "publish URLs without reading a byte of each first")
	}
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	o.check = cmd != "renew" && !noCheck
	switch {
	case fs.NArg() > 0:
		return options{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case o.config == "":
		return options{}, errors.New("-config is required")
	case cmd == "watch" && o.interval <= 0:
		return options{}, errors.New("-interval must be positive")
	}
	return o, nil
}

// loadKey reads the key from keyFile, else CIDSYNC_KEY. The key is never
// logged or printed.
func loadKey(keyFile string, getenv func(string) string) (string, error) {
	key := getenv("CIDSYNC_KEY")
	if keyFile != "" {
		b, err := os.ReadFile(keyFile)
		if err != nil {
			return "", err
		}
		key = strings.TrimSpace(string(b))
	}
	if key == "" {
		return "", errors.New("no key: set CIDSYNC_KEY or pass -key-file")
	}
	if strings.ContainsAny(key, " \t\r\n") {
		return "", errors.New("the key holds whitespace")
	}
	return key, nil
}

func newPublisher(o options, e env) (*publisher, error) {
	cfg, err := loadConfig(o.config)
	if err != nil {
		return nil, err
	}
	if o.section != nil && cfg.Sections[*o.section] == nil {
		return nil, fmt.Errorf("-section %q: not in the config", *o.section)
	}
	key, err := loadKey(o.keyFile, e.getenv)
	if err != nil {
		return nil, err
	}
	return &publisher{
		cfg:    cfg,
		client: &client{url: cfg.Index, key: key, http: e.http},
		http:   e.http,
		cache:  loadCache(cachePath(e.getenv)),
		log:    e.log,
		out:    e.stdout,
		dryRun: o.dryRun,
		force:  o.force,
		check:  o.check,
	}, nil
}

// runCID prints what a scan would publish of each file: its CID or, for a
// packed file or its descriptor, the anchor and the map CID.
func runCID(paths []string, e env) int {
	if len(paths) == 0 {
		fmt.Fprintln(e.stderr, usage)
		return exitConfig
	}
	code := exitOK
	for _, path := range paths {
		lines, err := cidLines(path)
		if err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			code = exitFailure
		}
		for _, line := range lines {
			fmt.Fprintln(e.stdout, line)
		}
	}
	return code
}

func cidLines(path string) ([]string, error) {
	packed := strings.TrimSuffix(path, proofsDir+string(filepath.Separator)+"root")
	if root, _ := packedRoot(packed); root != nil {
		p, err := readPacking(filepath.Join(packed+proofsDir, "root"))
		if err != nil {
			return nil, err
		}
		return []string{p.anchor + "  " + filepath.Join(packed+proofsDir, "root"), p.mapCID + "  " + packed}, nil
	}
	cid, _, err := fileCID(path)
	if err != nil {
		return nil, err
	}
	return []string{cid + "  " + path}, nil
}
