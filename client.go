package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// A section GET runs to about 50 MB: minutes, not seconds.
const requestTimeout = 5 * time.Minute

// Every write is absolute, so a request sent twice changes nothing: one
// that fails in transit or with a 5xx is sent again after each wait.
var retryWaits = []time.Duration{time.Second, 4 * time.Second, 16 * time.Second}

var errKey = errors.New("the index refused the key (401)")

// A client speaks to the index's section routes with one key.
type client struct {
	url  string // the index's, without a trailing slash
	key  string
	http *http.Client
}

// get reads a section as the index holds it. A section the index does not
// know is empty: the first patch creates it.
func (c *client) get(ctx context.Context, name string) (*stored, error) {
	res, err := c.do(ctx, http.MethodGet, name, nil)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	now := serverTime(res)
	if res.StatusCode == http.StatusNotFound {
		if err := apiError(res); err.Error() != "unknown_section" {
			return nil, fmt.Errorf("index: GET %s: %s: %w", sectionPath(name), res.Status, err)
		}
		return &stored{now: now}, nil
	}
	if res.StatusCode != http.StatusOK {
		return nil, c.failed(res, http.MethodGet, name)
	}
	st := &stored{exists: true, now: now}
	if err := json.NewDecoder(res.Body).Decode(st); err != nil {
		return nil, fmt.Errorf("index: GET %s: %w", sectionPath(name), err)
	}
	return st, nil
}

// patch writes a patch to a section, and returns the account's live
// offers.
func (c *client) patch(ctx context.Context, name string, body []byte) (int, error) {
	res, err := c.do(ctx, http.MethodPatch, name, body)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return 0, c.failed(res, http.MethodPatch, name)
	}
	var reply struct {
		Offers int `json:"offers"`
	}
	if err := json.NewDecoder(res.Body).Decode(&reply); err != nil {
		return 0, fmt.Errorf("index: PATCH %s: %w", sectionPath(name), err)
	}
	return reply.Offers, nil
}

// do sends a request until it gets an answer below 500; the last try's
// answer is returned as it is.
func (c *client) do(ctx context.Context, method, name string, body []byte) (*http.Response, error) {
	for _, wait := range retryWaits {
		res, err := c.send(ctx, method, name, body)
		if err == nil && res.StatusCode < http.StatusInternalServerError {
			return res, nil
		}
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
			res.Body.Close()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return c.send(ctx, method, name, body)
}

func (c *client) send(ctx context.Context, method, name string, body []byte) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+sectionPath(name), r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

// failed is the error for an answer other than the one wanted: errKey for
// a 401, else the status with the index's code and detail.
func (c *client) failed(res *http.Response, method, name string) error {
	if res.StatusCode == http.StatusUnauthorized {
		return errKey
	}
	return fmt.Errorf("index: %s %s: %s: %w", method, sectionPath(name), res.Status, apiError(res))
}

// apiError reads the index's error body: its code, then its detail.
func apiError(res *http.Response) error {
	var body struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&body)
	if body.Detail != "" {
		return fmt.Errorf("%s: %s", body.Error, body.Detail)
	}
	return errors.New(body.Error)
}

// serverTime is the reply's Date: the index checks every expiry against
// its own clock.
func serverTime(res *http.Response) time.Time {
	t, err := http.ParseTime(res.Header.Get("Date"))
	if err != nil {
		return time.Now().UTC().Truncate(time.Second)
	}
	return t.UTC()
}
