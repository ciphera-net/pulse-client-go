package credentials

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/zalando/go-keyring"
)

// * The environment beats the keychain, deliberately.
// *
// * A CI runner and a developer laptop are frequently the same machine. The
// * surprising failure is the one where a key that was deliberately exported for
// * this shell is silently ignored in favour of a stale stored one, and the user
// * spends the next ten minutes debugging permissions on a credential they are
// * not using. An explicit export is an explicit instruction.
func TestEnvironmentTakesPrecedenceOverTheKeychain(t *testing.T) {
	t.Setenv(EnvVar, "pulse_sk_live_from_the_environment")

	cred, err := Load("default")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cred.Key != "pulse_sk_live_from_the_environment" {
		t.Errorf("key = %q, want the exported one", cred.Key)
	}
	// * And it must SAY so, or `auth status` reports a keychain entry the user
	// * is not actually using.
	if cred.Source != SourceEnv {
		t.Errorf("source = %q, want %q — the user has to be able to tell which key is in play", cred.Source, SourceEnv)
	}
}

// * An empty export is not a credential. Treating "" as a key sends
// * `Authorization: Bearer ` and turns a configuration mistake into an
// * authentication error about the wrong thing.
func TestBlankEnvironmentValueIsNotACredential(t *testing.T) {
	t.Setenv(EnvVar, "   ")

	if cred, err := Load("nonexistent-profile-for-test"); err == nil && cred.Key != "" {
		t.Errorf("a blank %s was accepted as a key: %q", EnvVar, cred.Key)
	}
}

// * A pasted key that picked up a prompt character or a quote produces a 401
// * that reads as "your key is wrong" when the key is fine and the paste was
// * not. A shape check turns that into a message about the actual problem.
// *
// * Explicitly NOT validation — the API is the authority on whether a key works.
func TestLooksLikeKeyCatchesThePasteAccidents(t *testing.T) {
	if !LooksLikeKey("pulse_sk_live_abcdef") {
		t.Error("a well-formed key must be accepted")
	}
	if !LooksLikeKey("  pulse_sk_live_abcdef  ") {
		t.Error("surrounding whitespace is a paste artefact, not a malformed key")
	}

	for _, bad := range []string{
		`"pulse_sk_live_abcdef"`, // picked up quotes
		`$ pulse_sk_live_abcdef`, // picked up a shell prompt
		"sk_live_abcdef",         // a different product's key
		"",
	} {
		if LooksLikeKey(bad) {
			t.Errorf("%q should not look like a Pulse key", bad)
		}
	}
}

// * Redact shows only a tail, and never enough to reconstruct anything. It is
// * the fallback for when no /me response is available — the API's own last4 is
// * preferred wherever there is one, because the two are NOT the same string.
func TestRedactRevealsOnlyATail(t *testing.T) {
	const key = "pulse_sk_live_0123456789abcdefghijklmnop"

	got := Redact(key)
	// * Runes, not bytes: the ellipsis is three bytes in UTF-8.
	if !strings.HasSuffix(got, "mnop") || utf8.RuneCountInString(got) != 5 {
		t.Errorf("Redact(%q…) = %q, want an ellipsis and exactly four characters", key[:14], got)
	}
	if strings.Contains(got, "0123456789") {
		t.Errorf("Redact leaked the body of the key: %q", got)
	}
	if Redact("abc") == "abc" {
		t.Error("a short string must still be redacted rather than echoed")
	}
}

// * A machine with no keychain is NOT a broken machine.
// *
// * Found by running the real linux/amd64 binary in an Alpine container: with no
// * Secret Service on the session bus, go-keyring fails with "dbus-launch:
// * executable file not found", and the CLI exited 1 — telling a script the tool
// * had malfunctioned when the truth was that nobody had authenticated. That is
// * exactly the CI case the environment-variable fallback exists for, and exit 1
// * is the code a `|| [ $? -eq 3 ] && pulse auth login` guard never matches.
// *
// * No unit test could have found it: on a developer's macOS box the keychain is
// * always present, so the failing branch never runs.
func TestUnreadableKeychainCountsAsNoCredentialNotAsAFailure(t *testing.T) {
	t.Setenv(EnvVar, "")
	cause := errors.New(`exec: "dbus-launch": executable file not found in $PATH`)
	keyring.MockInitWithError(cause)
	t.Cleanup(keyring.MockInit)

	_, err := Load("default")
	if err == nil {
		t.Fatal("an unreadable keychain must still report that there is no credential")
	}
	if !errors.Is(err, ErrNoCredential) {
		t.Fatalf("err = %v; must satisfy errors.Is(err, ErrNoCredential) so the caller exits 3 rather than 1", err)
	}

	// * And the reason has to survive, or the user cannot tell "no keychain on
	// * this machine" from "your keychain is locked".
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("err = %T; want an *UnavailableError carrying the keychain's own reason", err)
	}
	if !strings.Contains(unavailable.Reason.Error(), "dbus-launch") {
		t.Errorf("the underlying reason was lost: %v", unavailable.Reason)
	}

	// * And it must be reachable through the standard error chain, not only as a
	// * struct field. Anyone handling this error is entitled to errors.Is against
	// * whatever the keyring backend returned — a wrapper type that reads like a
	// * wrapper but does not unwrap is worse than no wrapper, because the failure
	// * is silent.
	if !errors.Is(err, cause) {
		t.Error("the keychain's own error is not reachable via errors.Is — UnavailableError must unwrap to it")
	}
}

// * The complement: a keychain that works but holds nothing is the ordinary
// * first-run state and must be indistinguishable to the caller.
func TestEmptyKeychainIsAlsoNoCredential(t *testing.T) {
	t.Setenv(EnvVar, "")
	keyring.MockInit()

	_, err := Load("a-profile-that-was-never-stored")
	if !errors.Is(err, ErrNoCredential) {
		t.Fatalf("err = %v, want ErrNoCredential", err)
	}
}
