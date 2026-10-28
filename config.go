package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	defaultLifetime = 30 * 24 * time.Hour
	defaultRenew    = 7 * 24 * time.Hour
	maxLifetime     = 365 * 24 * time.Hour // the furthest expires the index takes
)

// A Config is the config file: the index's URL and the sections to keep
// in sync, by name. The name "" is the catalog, the account's root
// section.
type Config struct {
	Index    string              `json:"index"`
	Sections map[string]*Section `json:"sections"`
}

// A Section is a directory and the index section that offers its files.
// Base, Parent and Range are the section's settings, as the index takes
// them; unset, the section inherits them.
type Section struct {
	Dir      string   `json:"dir"`
	Base     string   `json:"base"`
	Parent   string   `json:"parent"`
	Range    *bool    `json:"range"`
	Lifetime string   `json:"lifetime"`
	Renew    string   `json:"renew"`
	Exclude  []string `json:"exclude"`

	lifetime, renew time.Duration // Lifetime and Renew, parsed
}

var sectionName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// loadConfig reads and checks a config file. A relative dir is relative to
// the file. Unknown fields are an error: the file is written by hand, and a
// typo must not pass silently.
func loadConfig(file string) (*Config, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cfg, err := decodeConfig(f)
	if err == nil {
		err = cfg.validate(filepath.Dir(file))
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return cfg, nil
}

func decodeConfig(r io.Reader) (*Config, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the config")
	}
	return &cfg, nil
}

func (c *Config) validate(configDir string) error {
	index, err := validIndex(c.Index)
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	c.Index = index
	if len(c.Sections) == 0 {
		return errors.New("sections: none")
	}
	for _, name := range sectionNames(c) {
		if err := c.Sections[name].validate(name, configDir); err != nil {
			return fmt.Errorf("section %q: %w", name, err)
		}
	}
	return nil
}

// sectionNames lists the config's sections in the order a sync takes
// them: each parent before its children, since the index wants a parent
// to exist first, and by name among equals, so the catalog comes first.
func sectionNames(c *Config) []string {
	names := slices.Sorted(maps.Keys(c.Sections))
	slices.SortStableFunc(names, func(a, b string) int { return depth(c, a) - depth(c, b) })
	return names
}

// depth counts a section's ancestors in the config, up to a bound that a
// cycle, which the index refuses, reaches.
func depth(c *Config, name string) int {
	d := 0
	for sec := c.Sections[name]; sec != nil && sec.Parent != "" && d <= len(c.Sections); sec = c.Sections[sec.Parent] {
		d++
	}
	return d
}

// validIndex returns the index's URL without a trailing slash. The key
// travels with every request, so plain http is for a local index only.
func validIndex(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", errors.New("no query, fragment or user info")
	}
	if u.Host == "" || u.Scheme != "https" && (u.Scheme != "http" || !loopback(u.Hostname())) {
		return "", errors.New("must be an https URL, or plain http to a loopback host")
	}
	return strings.TrimRight(raw, "/"), nil
}

func loopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}

func (s *Section) validate(name, configDir string) error {
	if name != "" && !sectionName.MatchString(name) {
		return errors.New("not a section name: 1 to 64 of a-z, 0-9 and -, not starting with -")
	}
	if err := s.resolveDir(configDir); err != nil {
		return fmt.Errorf("dir: %w", err)
	}
	if err := validBase(name, s.Base); err != nil {
		return fmt.Errorf("base: %w", err)
	}
	if err := validParent(name, s.Parent); err != nil {
		return fmt.Errorf("parent: %w", err)
	}
	if err := s.parseDurations(); err != nil {
		return err
	}
	for _, pattern := range s.Exclude {
		if _, err := path.Match(pattern, ""); err != nil {
			return fmt.Errorf("exclude %q: %w", pattern, err)
		}
	}
	return nil
}

// resolveDir makes Dir absolute, relative to the config file's directory,
// and checks it is a directory: a missing one is an error, never an empty
// section.
func (s *Section) resolveDir(configDir string) error {
	if s.Dir == "" {
		return errors.New("required")
	}
	dir, err := filepath.Abs(filepath.Join(configDir, s.Dir))
	if filepath.IsAbs(s.Dir) {
		dir, err = filepath.Clean(s.Dir), nil
	}
	if err != nil {
		return err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s: not a directory", dir)
	}
	s.Dir = dir
	return nil
}

// validBase checks what the index cannot: a base is a directory, and the
// catalog, with no parent to inherit one from, needs an absolute one. The
// index checks the rest.
func validBase(name, base string) error {
	if base == "" && name == "" {
		return errors.New("the catalog needs an absolute https base")
	}
	if base == "" {
		return nil
	}
	if !strings.HasSuffix(base, "/") {
		return errors.New("must end with /")
	}
	u, err := url.Parse(base)
	if err != nil {
		return err
	}
	switch {
	case u.IsAbs() && (u.Scheme != "https" || u.Host == ""):
		return errors.New("an absolute base must be an https URL")
	case u.IsAbs():
		return nil
	case name == "":
		return errors.New("the catalog needs an absolute https base")
	case strings.HasPrefix(base, "/"):
		return errors.New("a relative base is a path below the parent's base, without a leading /")
	}
	return nil
}

func validParent(name, parent string) error {
	switch {
	case parent == "":
		return nil
	case name == "":
		return errors.New("the catalog has none")
	case parent == name || !sectionName.MatchString(parent):
		return fmt.Errorf("%q: not another section's name", parent)
	}
	return nil
}

func (s *Section) parseDurations() error {
	var err error
	if s.lifetime, err = durationOr(s.Lifetime, defaultLifetime); err != nil {
		return fmt.Errorf("lifetime: %w", err)
	}
	if s.renew, err = durationOr(s.Renew, defaultRenew); err != nil {
		return fmt.Errorf("renew: %w", err)
	}
	if s.lifetime <= 0 || s.lifetime > maxLifetime {
		return errors.New("lifetime: must be positive and at most 365d")
	}
	if s.renew <= 0 || s.renew >= s.lifetime {
		return errors.New("renew: must be positive and below lifetime")
	}
	return nil
}

func durationOr(text string, fallback time.Duration) (time.Duration, error) {
	if text == "" {
		return fallback, nil
	}
	return parseDuration(text)
}

// parseDuration reads Go's durations plus a leading count of days, as in
// "30d" or "1d12h": lifetimes run to months.
func parseDuration(text string) (time.Duration, error) {
	days, rest, ok := strings.Cut(text, "d")
	if !ok {
		return time.ParseDuration(text)
	}
	n, err := strconv.ParseUint(days, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("%q: not a duration", text)
	}
	d := time.Duration(n) * 24 * time.Hour
	if rest == "" {
		return d, nil
	}
	more, err := time.ParseDuration(rest)
	if err != nil || more < 0 {
		return 0, fmt.Errorf("%q: not a duration", text)
	}
	return d + more, nil
}

// sectionPath is a section's route: the catalog's own, or one below
// /sections.
func sectionPath(name string) string {
	if name == "" {
		return "/catalog"
	}
	return "/sections/" + name
}
