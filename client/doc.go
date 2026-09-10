// Package client talks to the Pulse public read API.
//
// The API is aggregates-only and read-only: it answers questions about
// populations and refuses to answer questions about people. Two consequences
// shape every call made through this package, and both are easier to get wrong
// than to get right.
//
// # A withheld metric is not zero
//
// Every metric on [publicv1.Stats] is a pointer, and the server sets all of them
// to nil when a slice falls below its privacy floor — including when the true
// answer is zero. That last part is deliberate rather than sloppy: if
// Meta.Suppressed meant "between one and four" while a genuine zero came back as
// 0, then walking a dimension's values would reveal exactly which of them have a
// live cohort. A floor that leaks its own boundary is an existence oracle
// wearing a privacy floor's clothing.
//
// So nil means "fewer than Meta.MinCellSize, possibly none", and the caller
// cannot tell which. Rendering it as 0 publishes a number the server refused to
// state — and Go's zero value for a dereferenced-then-defaulted *int is exactly
// that wrong answer, which is what makes this the easiest mistake here to make.
// Read the threshold from Meta.MinCellSize rather than hardcoding it; that field
// travels on the wire precisely so a client can explain the gap without knowing
// our policy, and a hardcoded 5 describes the wrong floor the moment the server
// raises it.
//
// # The server owns the date range
//
// A relative period resolves in the SITE's timezone, not the caller's, and the
// window the server actually queried comes back in Meta.Range. Print that. A
// locally computed range silently disagrees with the numbers printed beside it
// whenever the site's timezone is not the reader's, and the disagreement is a
// day wide — large enough to matter and small enough to go unnoticed.
//
// # Paths are UUID-only
//
// Site identifiers in URLs are UUIDs, from [Sites]. A slug is user-editable, so
// putting one in a path means every stored command breaks the day somebody
// renames a site.
package client
