package client

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/ciphera-net/pulse-api-go/publicv1"
)

// Range is what the user asked for: a relative period, or explicit dates.
//
// Never both. The API answers 400 conflicting_range rather than letting one
// silently win, and this mirrors that locally — a caller sending both has a bug,
// and answering one of them hides it behind plausible numbers.
type Range struct {
	Period string // 7d | 30d | month | year
	From   string // YYYY-MM-DD
	To     string // YYYY-MM-DD
}

// NewRange validates what the flags produced.
func NewRange(period, from, to string) (Range, error) {
	r := Range{Period: strings.TrimSpace(period), From: strings.TrimSpace(from), To: strings.TrimSpace(to)}

	if r.Period != "" && (r.From != "" || r.To != "") {
		return Range{}, fmt.Errorf("use either --last, or --from and --to — not both")
	}
	if (r.From == "") != (r.To == "") {
		return Range{}, fmt.Errorf("--from and --to must be given together")
	}

	if r.Period != "" && !validPeriod(r.Period) {
		return Range{}, unsupportedPeriod(r.Period)
	}

	for _, d := range []struct{ name, value string }{{"--from", r.From}, {"--to", r.To}} {
		if d.value == "" {
			continue
		}
		if _, err := time.Parse("2006-01-02", d.value); err != nil {
			return Range{}, fmt.Errorf("%s must be a date in YYYY-MM-DD form (got %q)", d.name, d.value)
		}
	}
	if r.From != "" && r.From > r.To {
		// * String comparison is exact for zero-padded ISO dates, and both are
		// * already known to parse.
		return Range{}, fmt.Errorf("--from (%s) must be on or before --to (%s)", r.From, r.To)
	}

	return r, nil
}

// unsupportedPeriod explains the ones people reach for first.
//
// today, 24h, week and yesterday all exist in the dashboard, so a user who has
// seen the UI reasonably expects them. The API publishes a deliberately smaller
// vocabulary — every published period is a 24-month commitment — so the useful
// message names the alternative rather than just refusing.
func unsupportedPeriod(p string) error {
	msg := fmt.Sprintf("unsupported period %q\n\nsupported: %s", p, strings.Join(Periods, ", "))
	switch strings.ToLower(p) {
	case "today", "yesterday", "1h", "24h", "week", "1d", "1w":
		msg += fmt.Sprintf("\n\n%q is available in the dashboard but not in the API. Use --from and --to for that range.", p)
	}
	return fmt.Errorf("%s", msg)
}

// apply writes the range onto a query. An empty Range sends nothing, which the
// API reads as its documented default of 7d — and echoes back in meta.range, so
// the default is never silent.
func (r Range) apply(q url.Values) error {
	switch {
	case r.Period != "":
		q.Set("period", r.Period)
	case r.From != "":
		q.Set("from", r.From)
		q.Set("to", r.To)
	}
	return nil
}

// ResolveExportRange turns any Range into the explicit from/to the export
// endpoints require.
//
// The export endpoints do not accept period= — measured against production,
// they answer 400 "from and to date parameters are required". So a relative
// period has to become dates somewhere, and doing it locally would use the
// USER'S timezone to answer a question about the SITE's: `--last 7d` run from
// Tokyo would silently disagree with the same command run from Brussels, and
// with the dashboard, by a day at each edge.
//
// So the server resolves it. One extra request to /stats, one quota unit, and
// meta.range comes back carrying the dates the server itself would have used.
// The alternative — reimplementing ResolvePeriod client-side — is a second
// implementation of a calendar, which is the kind of thing that is correct until
// a month boundary or a DST transition.
func ResolveExportRange(ctx context.Context, c *Client, siteID string, r Range) (from, to string, tz string, err error) {
	if r.From != "" {
		return r.From, r.To, "", nil
	}

	period := r.Period
	if period == "" {
		period = "7d"
	}

	c.notify(fmt.Sprintf("resolving %s in the site's timezone (1 quota unit)", period))

	res, err := Stats(ctx, c, siteID, Range{Period: period}, nil)
	if err != nil {
		return "", "", "", err
	}
	if res.Meta.Range == nil {
		return "", "", "", fmt.Errorf("the API did not echo a resolved range for period %q; "+
			"cannot build an export without dates", period)
	}
	return res.Meta.Range.From, res.Meta.Range.To, res.Meta.Range.Timezone, nil
}

// DescribeRange renders what the SERVER said it queried.
//
// Always meta.range, never the flags the user typed. The two differ whenever a
// period was resolved, whenever a site's timezone is not the terminal's, and
// whenever the default applied — and printing the local guess is precisely the
// drift that server-side resolution exists to remove.
func DescribeRange(r *publicv1.Range) string {
	if r == nil {
		return ""
	}
	from, errFrom := time.Parse("2006-01-02", r.From)
	to, errTo := time.Parse("2006-01-02", r.To)
	if errFrom != nil || errTo != nil {
		return fmt.Sprintf("%s – %s (%s)", r.From, r.To, r.Timezone)
	}

	if from.Equal(to) {
		return fmt.Sprintf("%s (%s)", from.Format("2 Jan 2006"), r.Timezone)
	}
	if from.Year() == to.Year() {
		return fmt.Sprintf("%s – %s (%s)", from.Format("2 Jan"), to.Format("2 Jan 2006"), r.Timezone)
	}
	return fmt.Sprintf("%s – %s (%s)", from.Format("2 Jan 2006"), to.Format("2 Jan 2006"), r.Timezone)
}

func validPeriod(p string) bool {
	for _, known := range Periods {
		if known == p {
			return true
		}
	}
	return false
}
