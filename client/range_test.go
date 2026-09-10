package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// * The API publishes four periods. The dashboard knows nine.
// *
// * Every published period is a 24-month commitment, so the API deliberately
// * exposes a subset — and a user who has seen the dashboard reaches for `today`
// * or `24h` first. Catching that locally costs nothing and the message can name
// * the alternative; round-tripping it spends a request to be told less.
func TestOnlyTheAPIsPeriodsAreAccepted(t *testing.T) {
	for _, p := range []string{"7d", "30d", "month", "year"} {
		if _, err := NewRange(p, "", ""); err != nil {
			t.Errorf("period %q is published by the API and must be accepted: %v", p, err)
		}
	}

	// * Dashboard-only. Each must be refused, and the refusal must point at
	// * --from/--to rather than leaving the user with nowhere to go.
	for _, p := range []string{"today", "yesterday", "1h", "24h", "week"} {
		_, err := NewRange(p, "", "")
		if err == nil {
			t.Errorf("period %q is dashboard-only and must be refused locally", p)
			continue
		}
		if !strings.Contains(err.Error(), "--from") {
			t.Errorf("refusing %q must point at --from/--to: %v", p, err)
		}
	}

	if _, err := NewRange("90d", "", ""); err == nil {
		t.Error("an unpublished period must be refused")
	}
}

// * period and explicit dates are mutually exclusive, and answering one of them
// * would hide the caller's bug behind plausible numbers. The API returns 400
// * conflicting_range for the same reason; refusing locally keeps the two
// * surfaces telling the same story.
func TestPeriodAndExplicitDatesConflict(t *testing.T) {
	if _, err := NewRange("7d", "2026-08-01", "2026-08-07"); err == nil {
		t.Error("--last together with --from/--to must be refused, not silently resolved")
	}
	if _, err := NewRange("", "2026-08-01", ""); err == nil {
		t.Error("--from without --to must be refused")
	}
	if _, err := NewRange("", "", "2026-08-07"); err == nil {
		t.Error("--to without --from must be refused")
	}
}

// * An inverted range used to return a header-only CSV with a 200 on the API
// * side: a warehouse sync with its parameters swapped loaded nothing and
// * reconciled to zero forever, with no signal that anything was wrong.
func TestInvertedAndMalformedDatesAreRefused(t *testing.T) {
	if _, err := NewRange("", "2026-08-07", "2026-08-01"); err == nil {
		t.Error("from after to must be refused")
	}
	if _, err := NewRange("", "07-08-2026", "08-08-2026"); err == nil {
		t.Error("a non-ISO date must be refused")
	}
	if _, err := NewRange("", "2026-08-01", "2026-08-01"); err != nil {
		t.Errorf("a single-day range is legal: %v", err)
	}
}

// * The export endpoints do not accept period= — measured against production,
// * they answer 400 "from and to date parameters are required".
// *
// * So a relative period has to become dates somewhere, and it must NOT be here:
// * resolving locally would use the user's timezone to answer a question about
// * the site's, and `--last 7d` run from Tokyo would disagree with the same
// * command run from Brussels, and with the dashboard, at both edges.
// *
// * This asserts the CLI asks the SERVER and uses what it echoes back — the
// * fixture returns a range that no local clock would ever produce, so a
// * client-side calculation cannot accidentally pass.
func TestExportRangeIsResolvedByTheServer(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"visitors":1},"meta":{"range":{"from":"1999-01-01","to":"1999-01-07","timezone":"Pacific/Chatham","period":"7d"},"suppressed":false}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "pulse_sk_test", "test")
	from, to, tz, err := ResolveExportRange(context.Background(), c, "site-id", Range{Period: "7d"})
	if err != nil {
		t.Fatalf("ResolveExportRange: %v", err)
	}

	if from != "1999-01-01" || to != "1999-01-07" {
		t.Errorf("got %s..%s, want the server's echoed range 1999-01-01..1999-01-07 — a locally computed range would be today's", from, to)
	}
	if tz != "Pacific/Chatham" {
		t.Errorf("timezone = %q, want the server's", tz)
	}
	if !strings.Contains(gotPath, "/stats") || !strings.Contains(gotQuery, "period=7d") {
		t.Errorf("resolution must ask /stats with the period; got %s?%s", gotPath, gotQuery)
	}
}

// * Explicit dates are already absolute and must NOT cost an extra request.
func TestExplicitExportRangeCostsNoRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("explicit --from/--to must not trigger a resolution request")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, "pulse_sk_test", "test")
	from, to, _, err := ResolveExportRange(context.Background(), c, "site-id",
		Range{From: "2026-08-01", To: "2026-08-07"})
	if err != nil {
		t.Fatalf("ResolveExportRange: %v", err)
	}
	if from != "2026-08-01" || to != "2026-08-07" {
		t.Errorf("explicit dates were altered: %s..%s", from, to)
	}
}
