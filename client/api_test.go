package client

import (
	"net/url"
	"strings"
	"testing"
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
