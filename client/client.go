package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ciphera-net/pulse-api-go/publicv1"
)

// DefaultBaseURL is the production API. Overridable only through PULSE_API_URL,
// and deliberately not through a flag: a user-facing "point this at any host"
// option turns a support answer that begins "just set the URL to…" into one
// somebody else can also give.
const DefaultBaseURL = "https://pulse-api.ciphera.net/api/public/v1"

// Client talks to the Pulse public read API.
type Client struct {
	BaseURL   string
	APIKey    string
	UserAgent string
	HTTP      *http.Client

	// Notify receives human-readable progress notes — a retry, a spent quota
	// warning. It writes to stderr in normal use and is nil in tests.
	Notify func(string)
}

// New builds a client with the timeouts a CLI wants: long enough for a
// year-wide export, short enough that a hung connection does not look like a
// hung terminal.
func New(baseURL, apiKey, userAgent string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		APIKey:    apiKey,
		UserAgent: userAgent,
		HTTP:      &http.Client{Timeout: 60 * time.Second},
	}
}

// Result is one response: the decoded value, the bytes it was decoded from, and
// the headers.
//
// Raw is kept because --json promises "exactly the API response, unmodified".
// Re-marshalling the decoded struct would silently reorder keys, drop any field
// this build of the CLI does not know about, and turn the CLI into a second,
// subtly different contract — the specific outcome --json exists to prevent.
type Result[T any] struct {
	Data   T
	Meta   publicv1.Meta
	Raw    []byte
	Header http.Header
}

// Quota reports what the rate-limit headers said, if they were present.
func (r Result[T]) Quota() (limit, remaining int, ok bool) {
	l, errL := strconv.Atoi(r.Header.Get("X-RateLimit-Limit"))
	rem, errR := strconv.Atoi(r.Header.Get("X-RateLimit-Remaining"))
	if errL != nil || errR != nil {
		return 0, 0, false
	}
	return l, rem, true
}

// Get performs a GET and decodes the standard {data, meta} envelope.
func Get[T any](ctx context.Context, c *Client, path string, query url.Values) (*Result[T], error) {
	body, header, err := c.do(ctx, path, query, "application/json")
	if err != nil {
		return nil, err
	}

	var envelope publicv1.Envelope[T]
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("the API returned a body this version cannot read (%w); "+
			"if you are on an old build, upgrade — v1 only ever adds fields", err)
	}
	return &Result[T]{Data: envelope.Data, Meta: envelope.Meta, Raw: body, Header: header}, nil
}

// GetRaw performs a GET and returns the body untouched. For the export
// endpoints, which are a pre-envelope contract: CSV, or a bare JSON array.
func GetRaw(ctx context.Context, c *Client, path string, query url.Values, accept string) ([]byte, http.Header, error) {
	return c.do(ctx, path, query, accept)
}

// do issues the request, applying the one retry the rate limiter earns.
//
// A 429 is handled rather than surfaced: the API told us exactly how long to
// wait, and a CLI that gives up on information it was handed is worse than a
// human retrying by hand. Exactly one retry — a loop would turn a quota that is
// genuinely exhausted into a command that appears to hang.
func (c *Client) do(ctx context.Context, path string, query url.Values, accept string) ([]byte, http.Header, error) {
	body, header, err := c.attempt(ctx, path, query, accept)

	var apiErr *APIError
	if err != nil && errors.As(err, &apiErr) && apiErr.Status == http.StatusTooManyRequests {
		wait := time.Duration(apiErr.RetryAfter) * time.Second
		if wait <= 0 {
			wait = time.Second
		}
		if wait > 60*time.Second {
			// * Honour the header, but do not silently block a terminal for
			// * minutes. Above a minute the useful answer is the error.
			return nil, nil, apiErr
		}
		c.notify(fmt.Sprintf("rate limited; retrying once in %s", wait))

		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(wait):
		}
		return c.attempt(ctx, path, query, accept)
	}

	return body, header, err
}

func (c *Client) attempt(ctx context.Context, path string, query url.Values, accept string) ([]byte, http.Header, error) {
	endpoint := c.BaseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", c.UserAgent)
	// * No Origin header, ever. The API refuses any request carrying one with
	// * browser_origin_not_supported, which is correct: these keys are
	// * server-side credentials.

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("could not reach %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, nil, parseAPIError(resp.StatusCode, body, resp.Header)
	}
	return body, resp.Header, nil
}

func (c *Client) notify(msg string) {
	if c.Notify != nil {
		c.Notify(msg)
	}
}

// parseRetryAfter reads the delta-seconds form. The HTTP-date form is legal but
// the API sends seconds; an unparseable value yields 0 and the caller picks a
// floor rather than treating the header as absent.
func parseRetryAfter(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
