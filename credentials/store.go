// Package credentials stores the API key in the operating system's credential
// manager, and nowhere else.
//
// This is the part of the CLI that justifies the CLI. Today the documented way
// to use a Pulse API key is to put it in an environment variable, and the honest
// observation is that people put those in .env files and commit them. A keychain
// is only a better default if it is also the easier one, so `pulse auth login`
// stores to the keychain with no flag to opt in, and the CLI never writes a
// credential to a dotfile — not as a fallback, not on a keychain error.
package credentials

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/zalando/go-keyring"
)

// Service is the keychain service name. One entry per profile.
const Service = "ciphera-pulse"

// EnvVar is the CI escape hatch. A build agent has no keychain and no
// interactive session to unlock one, so an environment variable is the only
// thing that works there — but it stays an explicit fallback rather than the
// documented default.
const EnvVar = "PULSE_API_KEY"

// DefaultProfile is the profile used when --profile is not given.
const DefaultProfile = "default"

// Source says where a key came from, so `auth status` can tell the user
// something true about their own setup rather than just that a key exists.
type Source string

const (
	SourceKeyring Source = "keychain"
	SourceEnv     Source = "environment"
)

// Credential is a key and its provenance. The key itself is never logged,
// printed, or written anywhere by this package.
type Credential struct {
	Key     string
	Source  Source
	Profile string
}

// ErrNoCredential is returned when nothing is stored and nothing is exported.
var ErrNoCredential = errors.New("no API key found")

// UnavailableError is a keychain that could not be consulted at all.
//
// It counts as ErrNoCredential, deliberately. On a CI runner or a headless
// container there is no Secret Service on the session bus, and go-keyring fails
// with "dbus-launch: executable file not found" — which is not a malfunction,
// it is what that machine looks like. Treating it as an internal failure
// reports exit 1 where the truth is exit 3, and a script written as
// `pulse auth status || [ $? -eq 3 ] && pulse auth login` never fires.
//
// The underlying reason is kept and shown, because "there is no keychain here"
// and "your keychain is locked" need different responses from the user, and only
// the message can tell them apart.
type UnavailableError struct{ Reason error }

func (e *UnavailableError) Error() string {
	return "no API key found: " + e.Reason.Error()
}

// Is makes errors.Is(err, ErrNoCredential) true: from a caller's point of view
// an unreadable keychain and an empty one are the same situation.
func (e *UnavailableError) Is(target error) bool { return target == ErrNoCredential }

func (e *UnavailableError) Unwrap() error { return e.Reason }

// Load resolves a credential.
//
// The environment wins over the keychain deliberately. A CI job and a developer
// laptop can be the same machine, and the surprising failure is the one where a
// deliberately exported key is silently ignored in favour of a stale stored one.
// An explicit export is an explicit instruction.
func Load(profile string) (Credential, error) {
	profile = normalise(profile)

	if key := strings.TrimSpace(os.Getenv(EnvVar)); key != "" {
		return Credential{Key: key, Source: SourceEnv, Profile: profile}, nil
	}

	key, err := keyring.Get(Service, profile)
	switch {
	case errors.Is(err, keyring.ErrNotFound):
		return Credential{}, ErrNoCredential
	case err != nil:
		// * Unreadable, not broken. See UnavailableError: a machine with no
		// * Secret Service is the CI case this tool is meant to serve, and the
		// * honest report is "not authenticated" plus a reason, not "internal
		// * error".
		return Credential{}, &UnavailableError{Reason: keychainError("read", err)}
	case strings.TrimSpace(key) == "":
		return Credential{}, ErrNoCredential
	}
	return Credential{Key: key, Source: SourceKeyring, Profile: profile}, nil
}

// Store writes a key to the keychain.
//
// There is no file fallback. If the keychain is unavailable the correct answer
// is to say so and let the caller use the environment variable for this session
// — writing the key to disk to "be helpful" would recreate exactly the habit
// this package exists to break, and would do it silently.
func Store(profile, key string) error {
	if err := keyring.Set(Service, normalise(profile), key); err != nil {
		return keychainError("write to", err)
	}
	return nil
}

// Delete removes a stored key. Removing one that is not there is success: the
// user asked for the key to be gone, and it is.
func Delete(profile string) error {
	err := keyring.Delete(Service, normalise(profile))
	if err == nil || errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return keychainError("delete from", err)
}

// Redact renders a key for display when nothing better is available.
//
// Prefer the API's own Key.Last4 wherever a /me response is at hand. They are
// NOT the same string: measured against a live key, the API's last4 is not the
// final four characters of the credential, so showing the tail locally gives a
// user a value that matches nothing in the dashboard and makes "which key is
// this?" harder rather than easier.
func Redact(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= 4 {
		return "…"
	}
	return "…" + key[len(key)-4:]
}

// LooksLikeKey reports whether a pasted string has the shape of a Pulse key.
//
// A shape check, not validation — the API is the authority on whether a key
// works. It exists to catch the paste that picked up a shell prompt or a
// trailing quote, where the resulting 401 would otherwise read as "your key is
// wrong" when the key is fine and the paste was not.
func LooksLikeKey(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), "pulse_sk_")
}

func normalise(profile string) string {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return DefaultProfile
	}
	return profile
}

// keychainError explains a keychain failure in terms of the thing the user has
// to fix, which is different on each platform and is never obvious from the
// underlying error text.
func keychainError(verb string, err error) error {
	hint := ""
	switch runtime.GOOS {
	case "linux":
		hint = "\n\nOn Linux this needs a Secret Service provider (gnome-keyring or KWallet) on the session D-Bus. " +
			"Headless machines and containers usually have none — export " + EnvVar + " there instead."
	case "darwin":
		hint = "\n\nIf the login keychain is locked, unlock it in Keychain Access and try again."
	case "windows":
		hint = "\n\nThis uses the Windows Credential Manager; a locked or roaming profile can block access."
	}
	return fmt.Errorf("could not %s the system keychain: %w%s", verb, err, hint)
}
