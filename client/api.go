package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/ciphera-net/pulse-api-go/publicv1"
)

// Periods the API accepts. NOT the dashboard's vocabulary: 1h, 24h, today,
// yesterday and week are dashboard-only and return 400 unknown_period.
//
// Held as a list rather than inferred, so `--last today` fails locally with a
// message naming what works instead of spending a request to be told.
var Periods = []string{"7d", "30d", "month", "year"}

// FilterDimensions is the API's public allowlist, mirrored so a typo is caught
// before it costs a request. Deliberately a copy and not a guess: the internal
// dashboard accepts more (event_name among them), and the public surface keeps
// its own list precisely so it does not inherit every dimension ever added.
var FilterDimensions = []string{
	"page", "referrer", "channel", "country", "region", "city",
	"browser", "os", "device", "screen_resolution", "language", "timezone",
	"utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content",
}

// Me validates the calling key and describes it.
func Me(ctx context.Context, c *Client) (*Result[publicv1.Me], error) {
	return Get[publicv1.Me](ctx, c, "/me", nil)
}

// Sites lists what this key can read.
func Sites(ctx context.Context, c *Client) (*Result[[]publicv1.Site], error) {
	return Get[[]publicv1.Site](ctx, c, "/sites", nil)
}

// Stats is the aggregate over a range.
func Stats(ctx context.Context, c *Client, siteID string, r Range, filters []Filter) (*Result[publicv1.Stats], error) {
	q := url.Values{}
	if err := r.apply(q); err != nil {
		return nil, err
	}
	if err := applyFilters(q, filters); err != nil {
		return nil, err
	}
	return Get[publicv1.Stats](ctx, c, "/sites/"+siteID+"/stats", q)
}

// Realtime is the live view.
//
// It takes no filters and never will: a filtered five-minute window describes
// one person's current session. The API refuses the parameter outright, so
// there is nothing to pass through here.
func Realtime(ctx context.Context, c *Client, siteID string) (*Result[publicv1.Realtime], error) {
	return Get[publicv1.Realtime](ctx, c, "/sites/"+siteID+"/realtime", nil)
}

// ExportKind selects between the two bulk endpoints.
type ExportKind string

const (
	ExportDaily ExportKind = "daily"
	ExportPages ExportKind = "pages"
)

// ExportFormat is the server-side rendering. These endpoints predate the
// {data, meta} envelope and keep their own contract.
type ExportFormat string

const (
	ExportCSV  ExportFormat = "csv"
	ExportJSON ExportFormat = "json"
)

// Export fetches one of the bulk endpoints.
//
// from/to are REQUIRED here — unlike /stats, these endpoints do not accept
// period=. That asymmetry is why ResolveExportRange exists: the range still has
// to be resolved by the server, just by a different call.
func Export(ctx context.Context, c *Client, siteID string, kind ExportKind, from, to string,
	format ExportFormat, limit int, filters []Filter) ([]byte, http.Header, error) {

	q := url.Values{}
	q.Set("from", from)
	q.Set("to", to)
	q.Set("format", string(format))
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	if err := applyFilters(q, filters); err != nil {
		return nil, nil, err
	}

	accept := "text/csv"
	if format == ExportJSON {
		accept = "application/json"
	}
	return GetRaw(ctx, c, "/sites/"+siteID+"/export/"+string(kind), q, accept)
}

// Suppression describes what an export withheld. CSV has nowhere to put a meta
// object, so these arrive as headers. The pages export sends Rows/Pageviews;
// the daily export sends DayMetrics — the count of day buckets whose four
// per-session metrics were withheld (their CSV cells are empty, not zero).
type Suppression struct {
	Rows        int
	Pageviews   int
	DayMetrics  int
	MinCellSize int
}

// Suppressed reads the X-Pulse-Suppressed-* headers off an export response.
//
// PRESENCE is the signal, not the value. The server emits these headers
// whenever the privacy floor was in force — including as an explicit "0",
// precisely so a client can read "nothing was withheld" instead of inferring
// it from the headers' absence. Treating 0 as absent (as this function once
// did) rebuilt exactly the ambiguity the explicit zero exists to remove.
func Suppressed(h http.Header) (Suppression, bool) {
	present := false
	read := func(name string) int {
		v := h.Get(name)
		if v == "" {
			return 0
		}
		present = true
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
			return 0
		}
		return n
	}
	s := Suppression{
		Rows:        read("X-Pulse-Suppressed-Rows"),
		Pageviews:   read("X-Pulse-Suppressed-Pageviews"),
		DayMetrics:  read("X-Pulse-Suppressed-Day-Metrics"),
		MinCellSize: read("X-Pulse-Min-Cell-Size"),
	}
	return s, present
}

// Filter is one dimension==value or dimension!=value constraint.
type Filter struct {
	Dimension string
	Negated   bool
	Value     string
}

// ParseFilter reads the flag form, which is identical to the wire form so that
// what a user types is what gets sent.
func ParseFilter(expr string) (Filter, error) {
	for _, op := range []struct {
		token   string
		negated bool
	}{
		{"!=", true},
		{"==", false},
	} {
		if i := strings.Index(expr, op.token); i > 0 {
			f := Filter{
				Dimension: strings.TrimSpace(expr[:i]),
				Negated:   op.negated,
				Value:     expr[i+len(op.token):],
			}
			if f.Value == "" {
				return Filter{}, fmt.Errorf("filter %q has an empty value", expr)
			}
			if !knownDimension(f.Dimension) {
				return Filter{}, fmt.Errorf("unknown filter dimension %q\n\nsupported: %s",
					f.Dimension, strings.Join(FilterDimensions, ", "))
			}
			return f, nil
		}
	}
	return Filter{}, fmt.Errorf("filter must be dimension==value or dimension!=value, for example country==BE (got %q)", expr)
}

// applyFilters writes filters as REPEATED filter= parameters.
//
// Never comma-joined. Page paths and referrers routinely contain commas, pipes
// and percent signs, so a joined list needs an escaping scheme — the exact
// problem that forced the internal filter DSL to grow a v2: prefix and four
// escape sequences. Repeating a parameter has no in-band delimiter to get wrong.
func applyFilters(q url.Values, filters []Filter) error {
	if len(filters) == 0 {
		return nil
	}
	if err := CheckFilterDimensions(filters); err != nil {
		return err
	}
	for _, f := range filters {
		op := "=="
		if f.Negated {
			op = "!="
		}
		q.Add("filter", f.Dimension+op+f.Value)
	}
	return nil
}

// CheckFilterDimensions enforces the API's cap before spending a request.
//
// The cap counts DIMENSIONS, not filters: repeating one dimension ORs its
// values and broadens the query, while each new dimension ANDs and narrows it.
// Narrowing is what singles out, so ten --filter country== flags are fine and
// three different dimensions are not. Getting this wrong client-side would
// reject a legal query, which is worse than the round trip it saves.
func CheckFilterDimensions(filters []Filter) error {
	seen := map[string]bool{}
	for _, f := range filters {
		seen[f.Dimension] = true
	}
	if len(seen) <= publicv1.MaxFilterDimensions {
		return nil
	}

	names := make([]string, 0, len(seen))
	for d := range seen {
		names = append(names, d)
	}
	sort.Strings(names)

	return fmt.Errorf("an API key may combine at most %d filter dimensions; this uses %d (%s)\n\n"+
		"combining more narrows a query far enough to describe individuals rather than populations",
		publicv1.MaxFilterDimensions, len(seen), strings.Join(names, ", "))
}

func knownDimension(d string) bool {
	for _, known := range FilterDimensions {
		if known == d {
			return true
		}
	}
	return false
}
