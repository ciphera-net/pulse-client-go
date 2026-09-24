package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ciphera-net/pulse-api-go/publicv1"
)

// * Filters go on the wire as REPEATED filter= parameters, never comma-joined.
// *
// * The values are real page paths and referrers, which routinely contain
// * commas, pipes and percent signs. A joined list would need an escaping
// * scheme — exactly the problem that forced the internal filter DSL to grow a
// * v2: prefix and four escape sequences. A repeated parameter has no in-band
// * delimiter to get wrong.
// *
// * The path with a comma in it is the case that would silently split.
func TestFiltersAreRepeatedParametersNotAJoinedList(t *testing.T) {
	q := url.Values{}
	err := applyFilters(q, []Filter{
		{Dimension: "page", Value: "/pricing,plans"},
		{Dimension: "page", Value: "/about"},
	})
	if err != nil {
		t.Fatalf("applyFilters: %v", err)
	}

	got := q["filter"]
	if len(got) != 2 {
		t.Fatalf("got %d filter parameters %v, want 2 — a joined list splits the path containing a comma", len(got), got)
	}
	if got[0] != "page==/pricing,plans" {
		t.Errorf("first filter = %q; the comma inside the value must survive intact", got[0])
	}

	// * And the encoded query must carry two separate keys.
	if n := strings.Count(q.Encode(), "filter="); n != 2 {
		t.Errorf("encoded query has %d filter= keys, want 2: %s", n, q.Encode())
	}
}

// * The cap counts DIMENSIONS, not parameters.
// *
// * Repeating one dimension ORs its values and BROADENS the query; each new
// * dimension ANDs and NARROWS it, and narrowing is what singles out. Counting
// * parameters instead would reject a perfectly legal ten-country query while
// * still allowing the three-dimension fingerprint the cap exists to stop —
// * wrong in both directions at once.
func TestFilterCapCountsDimensionsNotParameters(t *testing.T) {
	tenValuesOneDimension := []Filter{}
	for _, c := range []string{"BE", "NL", "DE", "FR", "LU", "AT", "CH", "IT", "ES", "PT"} {
		tenValuesOneDimension = append(tenValuesOneDimension, Filter{Dimension: "country", Value: c})
	}
	if err := CheckFilterDimensions(tenValuesOneDimension); err != nil {
		t.Errorf("ten values on ONE dimension must be allowed — it broadens the query: %v", err)
	}

	twoDimensions := []Filter{
		{Dimension: "country", Value: "BE"},
		{Dimension: "browser", Value: "Firefox"},
	}
	if err := CheckFilterDimensions(twoDimensions); err != nil {
		t.Errorf("two dimensions is the documented maximum and must be allowed: %v", err)
	}

	threeDimensions := append(twoDimensions, Filter{Dimension: "os", Value: "macOS"})
	err := CheckFilterDimensions(threeDimensions)
	if err == nil {
		t.Fatal("three dimensions must be refused — that is the fingerprint the cap exists to prevent")
	}
	// * The message has to name the dimensions, or the user has to guess which
	// * flag to drop.
	for _, want := range []string{"country", "browser", "os"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
}

// * Both operators, because the API accepts both and a client that silently
// * drops != would send a filter that means the opposite of what was typed.
func TestParseFilterHandlesBothOperators(t *testing.T) {
	pos, err := ParseFilter("country==BE")
	if err != nil {
		t.Fatalf("country==BE: %v", err)
	}
	if pos.Dimension != "country" || pos.Value != "BE" || pos.Negated {
		t.Errorf("country==BE parsed as %+v", pos)
	}

	neg, err := ParseFilter("browser!=Firefox")
	if err != nil {
		t.Fatalf("browser!=Firefox: %v", err)
	}
	if !neg.Negated || neg.Dimension != "browser" || neg.Value != "Firefox" {
		t.Errorf("browser!=Firefox parsed as %+v", neg)
	}

	// * != is checked before ==, so a value that legitimately contains "==" is
	// * not mistaken for the operator.
	odd, err := ParseFilter("page==/a==b")
	if err != nil {
		t.Fatalf("page==/a==b: %v", err)
	}
	if odd.Value != "/a==b" {
		t.Errorf("value containing == was mis-split: %q", odd.Value)
	}
}

// * An unknown dimension is caught locally. The public allowlist is deliberately
// * narrower than the dashboard's — event_name is accepted by the internal DSL
// * and silently does nothing on this surface — so guessing from the dashboard
// * is a real failure mode, and the message must list what works.
func TestUnknownFilterDimensionIsRejectedWithTheList(t *testing.T) {
	_, err := ParseFilter("event_name==signup")
	if err == nil {
		t.Fatal("event_name must be refused: the public surface does not accept it")
	}
	if !strings.Contains(err.Error(), "country") {
		t.Errorf("refusal must list the supported dimensions: %v", err)
	}

	if _, err := ParseFilter("country=BE"); err == nil {
		t.Error("a single = is not the filter syntax and must be refused")
	}
	if _, err := ParseFilter("country=="); err == nil {
		t.Error("an empty filter value must be refused")
	}
}

// * The request must carry dimension=, the resolved range, every filter as its
// * own repeated filter= (never joined — see the filter tests above for why),
// * and limit= only when the caller actually set one.
func TestBreakdownRequestCarriesDimensionRangeFiltersAndLimit(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`{"data":{"dimension":"country","rows":[]},"meta":{"suppressed":false}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "pulse_sk_test", "test")
	r, err := NewRange("30d", "", "")
	if err != nil {
		t.Fatalf("NewRange: %v", err)
	}
	filters := []Filter{
		{Dimension: "browser", Value: "Firefox"},
		{Dimension: "country", Value: "BE"},
	}

	if _, err := Breakdown(context.Background(), c, "site-1", "country", r, filters, 50); err != nil {
		t.Fatalf("Breakdown: %v", err)
	}

	if got := gotQuery.Get("dimension"); got != "country" {
		t.Errorf("dimension = %q, want %q", got, "country")
	}
	if got := gotQuery.Get("period"); got != "30d" {
		t.Errorf("period = %q, want %q", got, "30d")
	}
	if got := gotQuery.Get("limit"); got != "50" {
		t.Errorf("limit = %q, want %q", got, "50")
	}
	if got := gotQuery["filter"]; len(got) != 2 {
		t.Fatalf("got %d filter parameters %v, want 2 — filters must be repeated, not joined", len(got), got)
	}

	// * from/to must reach the wire too, exactly like Stats.
	r2, err := NewRange("", "2026-08-01", "2026-08-07")
	if err != nil {
		t.Fatalf("NewRange: %v", err)
	}
	if _, err := Breakdown(context.Background(), c, "site-1", "page", r2, nil, 0); err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if got := gotQuery.Get("from"); got != "2026-08-01" {
		t.Errorf("from = %q, want %q", got, "2026-08-01")
	}
	if got := gotQuery.Get("to"); got != "2026-08-07" {
		t.Errorf("to = %q, want %q", got, "2026-08-07")
	}
	// * limit=0 means "use the server default" and must be OMITTED, not sent
	// * as a literal 0 — the server's own default (20) is not this client's to
	// * assert, and a stale "0" would silently ask for zero rows instead.
	if _, ok := gotQuery["limit"]; ok {
		t.Errorf("limit=0 sent a limit parameter (%v); it must be omitted so the server default applies", gotQuery["limit"])
	}
}

// * The decode side: a region row carries its ambiguous-without-it country
// * code, and an empty range decodes to an empty slice, never null — a client
// * that ranges over a nil slice is fine in Go, but the point of the contract is
// * that it never has to check.
func TestBreakdownDecodesRegionRowWithCountryAndEmptyRows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("dimension") {
		case "region":
			_, _ = w.Write([]byte(`{"data":{"dimension":"region","rows":[` +
				`{"value":"Limburg","country":"BE","visitors":12,"pageviews":30}` +
				`]},"meta":{"suppressed":false}}`))
		default:
			_, _ = w.Write([]byte(`{"data":{"dimension":"channel","rows":[]},"meta":{"suppressed":false}}`))
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "pulse_sk_test", "test")

	res, err := Breakdown(context.Background(), c, "site-1", "region", Range{}, nil, 0)
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if len(res.Data.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(res.Data.Rows))
	}
	row := res.Data.Rows[0]
	if row.Value != "Limburg" || row.Country == nil || *row.Country != "BE" {
		t.Errorf("region row = %+v, want value=Limburg country=BE", row)
	}
	if row.Visitors != 12 || row.Pageviews != 30 {
		t.Errorf("region row counts = %+v, want visitors=12 pageviews=30", row)
	}
	if res.Meta.Suppressed {
		t.Error("breakdown has no privacy floor; suppressed must be false")
	}

	empty, err := Breakdown(context.Background(), c, "site-1", "channel", Range{}, nil, 0)
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if empty.Data.Rows == nil {
		t.Error("rows decoded to nil, want an empty non-nil slice a caller can range over unguarded")
	}
	if len(empty.Data.Rows) != 0 {
		t.Errorf("got %d rows for an empty result, want 0", len(empty.Data.Rows))
	}
}

// * city, timezone and screen_resolution are filterable but deliberately not
// * groupable (see publicv1.BreakdownDimensions' own doc comment), and a typo
// * is just as real a mistake. Both must be caught locally — no HTTP call
// * spent finding out what the server already told this package at compile
// * time via publicv1.BreakdownDimensions().
func TestBreakdownRefusesAnUngroupableOrUnknownDimensionWithoutARequest(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"data":{"dimension":"x","rows":[]},"meta":{"suppressed":false}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "pulse_sk_test", "test")

	for _, dim := range []string{"city", "timezone", "screen_resolution", "utm_term", "utm_content", "event_name"} {
		_, err := Breakdown(context.Background(), c, "site-1", dim, Range{}, nil, 0)
		if err == nil {
			t.Errorf("dimension %q must be refused locally — it is filterable but not groupable", dim)
			continue
		}
		if !strings.Contains(err.Error(), "country") {
			t.Errorf("refusal for %q does not list the supported dimensions: %v", dim, err)
		}
	}

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("%d HTTP requests were made for dimensions that should have been refused locally", got)
	}

	// * And every real dimension must be accepted, straight from the wire
	// * contract's own list — nothing here should silently fall behind it.
	for _, dim := range publicv1.BreakdownDimensions() {
		if err := CheckBreakdownDimension(dim); err != nil {
			t.Errorf("CheckBreakdownDimension(%q) = %v, want nil — it is in publicv1.BreakdownDimensions()", dim, err)
		}
	}
}

// * limit is validated locally too: 0 is the sentinel for "omit", 1..100 is
// * the server's documented range, and everything else is refused before a
// * request is spent.
func TestBreakdownLimitBounds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"data":{"dimension":"page","rows":[]},"meta":{"suppressed":false}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "pulse_sk_test", "test")

	for _, limit := range []int{0, 1, 20, 100} {
		if _, err := Breakdown(context.Background(), c, "site-1", "page", Range{}, nil, limit); err != nil {
			t.Errorf("limit %d must be accepted: %v", limit, err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 4 {
		t.Errorf("made %d requests for 4 valid limits, want 4", got)
	}

	atomic.StoreInt32(&calls, 0)
	for _, limit := range []int{-1, 101, 1000} {
		if _, err := Breakdown(context.Background(), c, "site-1", "page", Range{}, nil, limit); err == nil {
			t.Errorf("limit %d must be refused — the API accepts only 1..100", limit)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("%d HTTP requests were made for out-of-range limits that should have been refused locally", got)
	}
}
