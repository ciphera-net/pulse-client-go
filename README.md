# pulse-client-go

The Go client for the [Pulse Analytics](https://ciphera.net/products/pulse) public read API, plus the
credential store the Pulse CLI uses.

```
go get github.com/ciphera-net/pulse-client-go
```

Two packages:

| Package | What it does |
|---|---|
| `client` | Talks to `https://pulse-api.ciphera.net/api/public/v1` — sites, stats, breakdowns, realtime, bulk exports. Retries, quota headers, typed errors. |
| `credentials` | Reads and writes the API key in the OS keychain, with a `PULSE_API_KEY` override. |

Wire types come from [`pulse-api-go`](https://github.com/ciphera-net/pulse-api-go), which the
Pulse backend imports too — so the types here are the server's own, not a reimplementation.

## Reading stats

```go
c := client.New("", apiKey, "my-app/1.0")

r, err := client.NewRange("7d", "", "")   // or ("", "2026-08-01", "2026-08-07")
if err != nil {
    return err
}

res, err := client.Stats(ctx, c, siteID, r, nil)
if err != nil {
    return err
}
```

`siteID` is a **UUID**, from `client.Sites`. Slugs and domains are user-editable and never belong
in a URL — a stored command would break the day somebody renames a site.

## Ranking a dimension

`client.Breakdown` is the top-N view behind `/sites/{id}/breakdown` — a site's traffic grouped by
one dimension and ranked largest first:

```go
res, err := client.Breakdown(ctx, c, siteID, "country", r, nil, 0) // limit=0: server default (20)
if err != nil {
    return err
}
for _, row := range res.Data.Rows {
    fmt.Println(row.Value, row.Visitors, row.Pageviews)
}
```

`dimension` is validated locally against `publicv1.BreakdownDimensions()` before a request is
spent — `client.CheckBreakdownDimension` is the same check, exported so a caller can validate a
flag value up front. `city`, `timezone`, `screen_resolution`, `utm_term` and `utm_content` are
filterable on `/stats` but **not** groupable here; they refuse locally with a message naming what
is. `limit` is `0` for "use the server default" (never sent on the wire) or `1..100`, also
checked before the request.

Region rows carry a `Country` alongside `Value`, because a region name alone is ambiguous —
`"Limburg"` is a province of both Belgium and the Netherlands.

**This endpoint has no privacy floor** (owner ruling, 24-09-2026): every row comes back with its
real counts, including rows with fewer than five visitors, and `res.Meta.Suppressed` is always
`false` here. `/stats` and the two exports keep theirs — see below.

## Two things that will bite you if you skip them

### A nil metric is not zero

Every metric on `publicv1.Stats` is a pointer, and the API sets them all to `nil` when a slice
falls below its privacy floor:

```go
if res.Meta.Suppressed {
    // Fewer than res.Meta.MinCellSize visitors — POSSIBLY NONE.
    // The API withholds a true zero as well, deliberately: if `suppressed`
    // meant "1 to 4" while a real zero came back as 0, iterating a dimension's
    // values would reveal which of them have a live cohort.
    return
}
fmt.Println(*res.Data.Visitors)
```

Rendering a `nil` metric as `0` publishes a number the server refused to state. Go's zero value
for a dereferenced-then-defaulted `*int` is exactly the wrong answer, which makes this the
easiest mistake in the library to make.

Read the threshold from `res.Meta.MinCellSize` rather than hardcoding it — that field is on the
wire precisely so a client can explain the gap without knowing our policy.

### The server owns the date range

`period=` resolves in the **site's** timezone, not yours, and the resolved window comes back in
`res.Meta.Range`. Print that. A locally computed range silently disagrees with the numbers
beside it whenever the site's timezone is not the reader's.

## Credentials

```go
cred, err := credentials.Load(credentials.DefaultProfile)
```

`PULSE_API_KEY` wins if set; otherwise the OS keychain. There is **no file fallback** — a key on
disk outlives the decision to store it, and a headless machine with no keychain should say so
rather than quietly writing one. `credentials.UnavailableError` distinguishes "no key stored"
from "no keychain on this machine" from "keychain unreadable", because those need different
answers.

## Errors

`client.APIError` carries the API's `type`, `code` and `message`, and an `ExitCode()` a CLI can
return directly. Rate limiting is retried automatically, honouring `Retry-After`.

## Licence

Apache-2.0. © Ciphera BV.
