// Package httpx is the small HTTP layer shared by the Jira and Bitbucket clients: bearer-token
// auth, JSON in/out, timeouts, and bounded retries for the failures that are worth retrying
// (rate limiting and gateway errors), with errors that say what to check.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	BaseURL    string // no trailing slash
	Token      string // personal access token / HTTP access token (sent as a Bearer token)
	HTTP       *http.Client
	MaxRetries int                 // retries after the first attempt (default 3)
	Sleep      func(time.Duration) // injectable for tests (default time.Sleep)
	Name       string              // "Jira" / "Bitbucket", used in error messages
}

type APIError struct {
	Name, Method, Path string
	Status             int
	Body               string
}

func (e *APIError) Error() string {
	hint := ""
	switch e.Status {
	case 401:
		hint = " (the token was rejected: check it is valid and has not expired)"
	case 403:
		hint = " (the token's user lacks permission for this)"
	case 404:
		hint = " (not found, or the token's user cannot see it: Atlassian returns 404 for things you may not access)"
	}
	return fmt.Sprintf("%s %s %s: HTTP %d%s: %s", e.Name, e.Method, e.Path, e.Status, hint, e.Body)
}

func retryable(status int) bool {
	return status == 429 || status == 502 || status == 503 || status == 504
}

// DoJSON sends in (if non-nil) as JSON and decodes the response into out (if non-nil).
func (c *Client) DoJSON(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = b, "application/json"
	}
	return c.Do(ctx, method, path, body, ct, nil, out)
}

// Do sends a raw body. Retries are safe because the body is re-sent from memory each time.
func (c *Client) Do(ctx context.Context, method, path string, body []byte, contentType string, headers map[string]string, out any) error {
	retries := c.MaxRetries
	if retries == 0 {
		retries = 3
	}
	sleep := c.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	name := c.Name
	if name == "" {
		name = "HTTP"
	}
	var last error
	for attempt := 0; attempt <= retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("Accept", "application/json")
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := hc.Do(req)
		var wait time.Duration
		switch {
		case err != nil:
			last = fmt.Errorf("%s %s %s: %w", name, method, path, err)
		default:
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				if out != nil && len(bytes.TrimSpace(raw)) > 0 {
					if err := json.Unmarshal(raw, out); err != nil {
						return fmt.Errorf("%s %s %s: bad JSON response: %w", name, method, path, err)
					}
				}
				return nil
			}
			text := string(raw)
			if len(text) > 300 {
				text = text[:300] + "…"
			}
			last = &APIError{Name: name, Method: method, Path: path, Status: resp.StatusCode, Body: text}
			if !retryable(resp.StatusCode) {
				return last
			}
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
				wait = time.Duration(s) * time.Second
			}
		}
		if attempt == retries {
			break
		}
		if wait == 0 {
			wait = 200 * time.Millisecond << attempt
		}
		if wait > 30*time.Second {
			wait = 30 * time.Second
		}
		sleep(wait)
	}
	return last
}
