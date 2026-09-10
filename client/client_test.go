package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ciphera-net/pulse-api-go/publicv1"
)

// * Exit codes are a published contract: scripts branch on them. Each error.type
// * maps to exactly one code, and getting the mapping wrong turns a retryable
// * rate limit into "your credentials are broken".
func TestExitCodesMapFromErrorType(t *testing.T) {
	for _, tc := range []struct {
		errType string
		want    int
	}{
		{publicv1.ErrorTypeInvalidRequest, ExitInvalidInput},
		{publicv1.ErrorTypeUnauthorized, ExitUnauthorized},
		{publicv1.ErrorTypeForbidden, ExitUnauthorized},
		{publicv1.ErrorTypeNotFound, ExitNotFound},
		{publicv1.ErrorTypeRateLimited, ExitRateLimited},
		{publicv1.ErrorTypeServerError, ExitServerError},
	} {
		e := &APIError{Type: tc.errType, Status: 400}
		if got := e.ExitCode(); got != tc.want {
			t.Errorf("error.type %q exited %d, want %d", tc.errType, got, tc.want)
		}
	}
}

// * An error type this build has never heard of is a server error, per the
// * contract's own instruction. Silently exiting 0 on an unrecognised refusal
// * would let a script treat a failure as a result — v1 is additive, so meeting
// * an unfamiliar value is the expected case, not the exceptional one.
func TestUnknownErrorTypeIsNotSuccess(t *testing.T) {
	e := &APIError{Type: "teapot", Status: 418}
	if got := e.ExitCode(); got == ExitOK {
		t.Fatal("an unrecognised error.type must never exit 0")
	}
}

// * Not every refusal is ours. A key rejected at the edge came back as an
// * openresty HTML 400, and Bunny's WAF answers a suspicious query with its own
// * 403 before the origin sees it — both measured against production. A client
// * that assumes every non-2xx decodes into publicv1.Error reports nothing
// * useful for exactly the failures a user cannot diagnose themselves.
func TestNonJSONRefusalStillProducesTheRightExitCode(t *testing.T) {
	html := []byte("<html><head><title>400 Bad Request</title></head><body>openresty</body></html>")

	for _, tc := range []struct {
		status int
		want   int
	}{
		{http.StatusBadRequest, ExitInvalidInput},
		{http.StatusUnauthorized, ExitUnauthorized},
		{http.StatusForbidden, ExitUnauthorized},
		{http.StatusNotFound, ExitNotFound},
		{http.StatusTooManyRequests, ExitRateLimited},
		{http.StatusBadGateway, ExitServerError},
	} {
		e := parseAPIError(tc.status, html, http.Header{})
		if got := e.ExitCode(); got != tc.want {
			t.Errorf("HTML %d exited %d, want %d", tc.status, got, tc.want)
		}
		// * And the message must not paste an HTML page into the terminal.
		if strings.Contains(e.Error(), "<html>") {
			t.Errorf("HTML %d leaked markup into the message: %q", tc.status, e.Error())
		}
		if !strings.Contains(e.Error(), "proxy") && !strings.Contains(e.Error(), "WAF") {
			t.Errorf("HTML %d should say the response was not the API's: %q", tc.status, e.Error())
		}
	}
}

// * A 429 is handled, not surfaced. The API told us exactly how long to wait, so
// * giving up on information we were handed is worse than a human retrying by
// * hand.
// *
// * Exactly ONCE, though: a loop turns a genuinely exhausted quota into a
// * command that appears to hang, and the useful answer then is the error.
func TestRateLimitRetriesExactlyOnce(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"rate_limited","code":"quota_exceeded","message":"slow down"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"visitors":7},"meta":{"suppressed":false}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "pulse_sk_test", "test")
	var notes []string
	c.Notify = func(s string) { notes = append(notes, s) }

	res, err := Get[publicv1.Stats](context.Background(), c, "/stats", nil)
	if err != nil {
		t.Fatalf("a single 429 must be absorbed by the retry: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("made %d requests, want exactly 2 (original + one retry)", got)
	}
	if res.Data.Visitors == nil || *res.Data.Visitors != 7 {
		t.Errorf("retry returned the wrong body: %+v", res.Data)
	}
	// * The retry has to be visible. A silent pause looks like a hang.
	if len(notes) == 0 || !strings.Contains(strings.ToLower(notes[0]), "rate limited") {
		t.Errorf("the retry must be announced on stderr; notes = %v", notes)
	}
}

// * A second 429 is a genuinely exhausted quota: stop, and exit 5.
func TestSecondRateLimitFailsWithExit5(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limited","code":"quota_exceeded","message":"quota exhausted"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "pulse_sk_test", "test")
	c.Notify = func(string) {}

	// * A deadline, so that "retries forever" fails as an assertion rather than
	// * hanging the suite until the package timeout. A guard that stalls reads as
	// * a broken test run, and the reflex is to rerun it rather than to look.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := Get[publicv1.Stats](ctx, c, "/stats", nil)
	if err == nil {
		t.Fatal("two 429s in a row must fail")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want an *APIError after exactly one retry, got %T (%v) — "+
			"a deadline here means the client kept retrying instead of giving up", err, err)
	}
	if got := apiErr.ExitCode(); got != ExitRateLimited {
		t.Errorf("exit code %d, want %d", got, ExitRateLimited)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("made %d requests, want 2 — it must not keep retrying", got)
	}
}

// * --json promises "exactly the API response, unmodified". Keeping the raw
// * bytes rather than re-marshalling the decoded struct is what makes the CLI a
// * debugging tool for the API instead of a second, subtly different contract.
// *
// * The fixture carries a field this build does not know about and unusual key
// * ordering — both of which a re-encode would destroy, and v1 being
// * additive-only means an old CLI meeting a new field is the NORMAL case.
func TestRawBodyIsPreservedByteForByte(t *testing.T) {
	body := `{"meta":{"suppressed":false},"data":{"visitors":42,"a_field_from_the_future":"kept"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := New(srv.URL, "pulse_sk_test", "test")
	res, err := Get[publicv1.Stats](context.Background(), c, "/stats", nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if string(res.Raw) != body {
		t.Errorf("raw body was altered.\n got: %s\nwant: %s", res.Raw, body)
	}
	if !strings.Contains(string(res.Raw), "a_field_from_the_future") {
		t.Error("an unrecognised field was dropped — --json must survive additive changes")
	}
	// * And the decode still worked for the fields we do know.
	if res.Data.Visitors == nil || *res.Data.Visitors != 42 {
		t.Errorf("decoding failed alongside the passthrough: %+v", res.Data)
	}
}

// * A suppressed body must decode to nil pointers, not to zeros. This is the
// * same guarantee publicv1's own tests make on the encode side, asserted here
// * on the decode side because that is where the CLI actually reads it.
func TestSuppressedBodyDecodesToNilNotZero(t *testing.T) {
	body := `{"data":{"visitors":null,"pageviews":null,"bounce_rate":null,"avg_duration":null,` +
		`"avg_scroll_depth":null,"avg_visible_duration":null},"meta":{"suppressed":true,"min_cell_size":5}}`

	var env publicv1.StatsEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Data.Visitors != nil {
		t.Errorf("withheld visitors decoded to %d, want nil", *env.Data.Visitors)
	}
	if !env.Meta.Suppressed {
		t.Error("meta.suppressed lost in decoding")
	}
	if env.Meta.MinCellSize != 5 {
		t.Errorf("min_cell_size = %d, want 5", env.Meta.MinCellSize)
	}
}

// * The credential goes in the Authorization header and an Origin is never sent.
// * The API answers any request carrying an Origin with 400
// * browser_origin_not_supported, correctly — these keys are server-side only.
func TestRequestCarriesBearerAndNoOrigin(t *testing.T) {
	var auth, origin, ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, origin, ua = r.Header.Get("Authorization"), r.Header.Get("Origin"), r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{"data":{},"meta":{"suppressed":false}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "pulse_sk_live_example", "pulse-cli/1.2.3")
	if _, err := Get[publicv1.Stats](context.Background(), c, "/stats", nil); err != nil {
		t.Fatalf("get: %v", err)
	}

	if auth != "Bearer pulse_sk_live_example" {
		t.Errorf("Authorization = %q", auth)
	}
	if origin != "" {
		t.Errorf("an Origin header was sent (%q); the API refuses those and it would break every call", origin)
	}
	if ua != "pulse-cli/1.2.3" {
		t.Errorf("User-Agent = %q, want the versioned CLI agent", ua)
	}
}

// * Quota headers are how `auth status` answers "how much is left" without a
// * second endpoint.
func TestQuotaIsReadFromHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "250000")
		w.Header().Set("X-RateLimit-Remaining", "235644")
		_, _ = w.Write([]byte(`{"data":{},"meta":{"suppressed":false}}`))
	}))
	defer srv.Close()

	res, err := Get[publicv1.Stats](context.Background(), New(srv.URL, "k", "test"), "/stats", nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	limit, remaining, ok := res.Quota()
	if !ok || limit != 250000 || remaining != 235644 {
		t.Errorf("quota = (%d, %d, %v), want (250000, 235644, true)", limit, remaining, ok)
	}

	// * Absent headers must report "unknown", not zero — a client that renders a
	// * missing header as 0 tells the user their quota is exhausted.
	if _, _, ok := (Result[publicv1.Stats]{Header: http.Header{}}).Quota(); ok {
		t.Error("absent quota headers must report ok=false rather than zero")
	}
}

// * The server sends the suppression headers whenever the floor was in force,
// * INCLUDING as an explicit "0" — so a client can read "nothing was withheld"
// * rather than inferring it from absence. An explicit zero must therefore
// * come back present-with-zero, not vanish. This function used to return
// * false on rows==0, silently rebuilding the infer-from-absence ambiguity.
func TestSuppressedExplicitZeroIsPresent(t *testing.T) {
	h := http.Header{}
	h.Set("X-Pulse-Suppressed-Rows", "0")
	h.Set("X-Pulse-Suppressed-Pageviews", "0")
	h.Set("X-Pulse-Min-Cell-Size", "5")

	s, ok := Suppressed(h)
	if !ok {
		t.Fatal("explicit-zero suppression headers were dropped as absent")
	}
	if s.Rows != 0 || s.Pageviews != 0 || s.MinCellSize != 5 {
		t.Errorf("explicit zeros mis-read: %+v", s)
	}
}

// * No headers at all means the floor was not in force (an unfiltered pages
// * export, a pre-floor server). That — and only that — is "no signal".
func TestSuppressedAbsentHeadersMeanNoSignal(t *testing.T) {
	if s, ok := Suppressed(http.Header{}); ok {
		t.Errorf("absent headers reported as present: %+v", s)
	}
}

// * The daily export signals with X-Pulse-Suppressed-Day-Metrics instead of
// * Rows/Pageviews. Its explicit zero carries the same weight.
func TestSuppressedReadsDailyExportHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-Pulse-Suppressed-Day-Metrics", "3")
	h.Set("X-Pulse-Min-Cell-Size", "5")

	s, ok := Suppressed(h)
	if !ok {
		t.Fatal("daily-export suppression headers were not recognised")
	}
	if s.DayMetrics != 3 || s.Rows != 0 || s.MinCellSize != 5 {
		t.Errorf("daily headers mis-read: %+v", s)
	}

	h.Set("X-Pulse-Suppressed-Day-Metrics", "0")
	if _, ok := Suppressed(h); !ok {
		t.Error("an explicit-zero day-metrics header was dropped as absent")
	}
}
