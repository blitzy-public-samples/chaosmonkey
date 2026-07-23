// Copyright 2016 Netflix, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package argocd

// White-box, table-driven tests for config.go. They lock in the credential and
// backward-compatibility safety fixes: disabled inertness (no file I/O), token
// trimming and empty-credential rejection when enabled, token-file precedence,
// bounded timeout conversion, Application normalization, and token redaction in
// formatted output. They use only the standard-library testing package, matching
// every other test in this module (no test file imports testify; it is an
// indirect-only go.mod entry whose direct use would force a go.mod/go.sum change
// the minimal-change contract forbids).

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Netflix/chaosmonkey/v2/config"
	"github.com/Netflix/chaosmonkey/v2/config/param"
)

// writeTokenFile writes content to a temp file and returns its path.
func writeTokenFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "argocd-token")
	if err := ioutil.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("failed to write temp token file: %v", err)
	}
	return path
}

// TestConfigFromMonkey_DisabledIsInertAndReadsNothing verifies that a disabled
// integration returns an inert snapshot WITHOUT reading token_file — even when
// token_file points at a nonexistent path (the M-03 backward-compat fix).
func TestConfigFromMonkey_DisabledIsInertAndReadsNothing(t *testing.T) {
	m := config.Defaults() // argocd.enabled defaults to false
	m.Set(param.ArgoCDTokenFile, "/nonexistent/definitely/not/here/token")
	m.Set(param.ArgoCDApplications, "not-a-valid-slice-encoding{{")

	c, err := configFromMonkey(m)
	if err != nil {
		t.Fatalf("disabled config must not error (no file/applications I/O), got: %v", err)
	}
	if c.Enabled {
		t.Error("disabled config must have Enabled=false")
	}
	if c.token != "" {
		t.Errorf("disabled config must not resolve a token, got %q", c.token)
	}
	if len(c.Applications) != 0 {
		t.Errorf("disabled config must not read applications, got %v", c.Applications)
	}
}

// TestConfigFromMonkey_EnabledInlineTokenTrimmed verifies the inline token is
// trimmed and the snapshot is populated (m-01).
func TestConfigFromMonkey_EnabledInlineTokenTrimmed(t *testing.T) {
	m := config.Defaults()
	m.Set(param.ArgoCDEnabled, true)
	m.Set(param.ArgoCDEndpoint, "https://argocd.example.com")
	m.Set(param.ArgoCDToken, "  tok-abc  ")

	c, err := configFromMonkey(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.Enabled {
		t.Error("Enabled = false, want true")
	}
	if c.token != "tok-abc" {
		t.Errorf("token = %q, want %q (must be trimmed)", c.token, "tok-abc")
	}
}

// TestConfigFromMonkey_EnabledEmptyTokenErrors verifies that enabling the
// integration without any resolvable credential is rejected, so an empty bearer
// header is never formed (m-01, fail-closed).
func TestConfigFromMonkey_EnabledEmptyTokenErrors(t *testing.T) {
	cases := []struct {
		name  string
		token string
	}{
		{"no token at all", ""},
		{"whitespace-only token", "   \t  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := config.Defaults()
			m.Set(param.ArgoCDEnabled, true)
			m.Set(param.ArgoCDEndpoint, "https://argocd.example.com")
			m.Set(param.ArgoCDToken, tc.token)

			if _, err := configFromMonkey(m); err == nil {
				t.Error("expected an error for an enabled config with an empty resolved token, got nil")
			}
		})
	}
}

// TestConfigFromMonkey_TokenFilePrecedence verifies a non-empty token_file wins
// over the inline token and its content is trimmed.
func TestConfigFromMonkey_TokenFilePrecedence(t *testing.T) {
	m := config.Defaults()
	m.Set(param.ArgoCDEnabled, true)
	m.Set(param.ArgoCDEndpoint, "https://argocd.example.com")
	m.Set(param.ArgoCDToken, "inline-token")
	m.Set(param.ArgoCDTokenFile, writeTokenFile(t, "  file-token\n"))

	c, err := configFromMonkey(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.token != "file-token" {
		t.Errorf("token = %q, want %q (token_file must take precedence and be trimmed)", c.token, "file-token")
	}
}

// TestConfigFromMonkey_TokenFileReadErrorWhenEnabled verifies that an enabled
// integration with an unreadable token_file surfaces a genuine error.
func TestConfigFromMonkey_TokenFileReadErrorWhenEnabled(t *testing.T) {
	m := config.Defaults()
	m.Set(param.ArgoCDEnabled, true)
	m.Set(param.ArgoCDEndpoint, "https://argocd.example.com")
	m.Set(param.ArgoCDTokenFile, "/nonexistent/definitely/not/here/token")

	if _, err := configFromMonkey(m); err == nil {
		t.Error("expected an error reading a nonexistent token_file when enabled, got nil")
	}
}

// TestConfigFromMonkey_NilConfigFailsClosed verifies that a nil *config.Monkey
// fails closed with a descriptive error rather than panicking, so a mis-wired
// caller disables the feature instead of crashing the scheduler. (Nil guard
// added per code review M-04.)
func TestConfigFromMonkey_NilConfigFailsClosed(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("configFromMonkey(nil) panicked: %v, want a returned error", r)
		}
	}()
	c, err := configFromMonkey(nil)
	if err == nil {
		t.Error("configFromMonkey(nil) error = nil, want a non-nil error")
	}
	if c.Enabled {
		t.Error("configFromMonkey(nil) returned an Enabled config, want the inert zero value")
	}
}

// TestConfigFromMonkey_OversizedTokenFileRejected verifies the M-04 fail-closed
// bound: a token_file larger than maxTokenFileBytes is rejected with an error
// rather than being fully buffered, so an oversized or adversarial file cannot
// exhaust memory.
func TestConfigFromMonkey_OversizedTokenFileRejected(t *testing.T) {
	// One byte past the cap is enough to trip the fail-closed guard.
	oversized := strings.Repeat("a", maxTokenFileBytes+1)

	m := config.Defaults()
	m.Set(param.ArgoCDEnabled, true)
	m.Set(param.ArgoCDEndpoint, "https://argocd.example.com")
	m.Set(param.ArgoCDTokenFile, writeTokenFile(t, oversized))

	_, err := configFromMonkey(m)
	if err == nil {
		t.Fatal("configFromMonkey with an oversized token_file error = nil, want an over-limit error")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error = %q, want it to mention the size limit", err.Error())
	}
}

// TestConfigFromMonkey_ApplicationsNormalized verifies eligible Application
// names are trimmed, de-duplicated, and emptied entries dropped (M-02).
func TestConfigFromMonkey_ApplicationsNormalized(t *testing.T) {
	m := config.Defaults()
	m.Set(param.ArgoCDEnabled, true)
	m.Set(param.ArgoCDEndpoint, "https://argocd.example.com")
	m.Set(param.ArgoCDToken, "tok")
	m.Set(param.ArgoCDApplications, []string{"  a ", "", "a", "b", "   "})

	c, err := configFromMonkey(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"a", "b"}
	if len(c.Applications) != len(want) {
		t.Fatalf("Applications = %v, want %v", c.Applications, want)
	}
	for i := range want {
		if c.Applications[i] != want[i] {
			t.Errorf("Applications[%d] = %q, want %q", i, c.Applications[i], want[i])
		}
	}
}

// TestConfigFromMonkey_TimeoutBounds verifies the seconds->duration conversion
// through the public config path defaults, passes through, and clamps.
func TestConfigFromMonkey_TimeoutBounds(t *testing.T) {
	cases := []struct {
		name string
		secs int
		want time.Duration
	}{
		{"non-positive falls back to default", 0, defaultTimeoutSeconds * time.Second},
		{"negative falls back to default", -5, defaultTimeoutSeconds * time.Second},
		{"passthrough", 45, 45 * time.Second},
		{"exactly max", maxTimeoutSeconds, maxTimeoutSeconds * time.Second},
		{"over max is clamped", maxTimeoutSeconds + 1000, maxTimeoutSeconds * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := config.Defaults()
			m.Set(param.ArgoCDEnabled, true)
			m.Set(param.ArgoCDEndpoint, "https://argocd.example.com")
			m.Set(param.ArgoCDToken, "tok")
			m.Set(param.ArgoCDTimeout, tc.secs)

			c, err := configFromMonkey(m)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.Timeout != tc.want {
				t.Errorf("Timeout = %s, want %s", c.Timeout, tc.want)
			}
			if c.Timeout <= 0 {
				t.Errorf("Timeout must be positive, got %s", c.Timeout)
			}
		})
	}
}

// TestTimeoutFromSeconds_OverflowSafe verifies the bound prevents an int64
// overflow into a non-positive duration for extreme inputs (m-02).
func TestTimeoutFromSeconds_OverflowSafe(t *testing.T) {
	cases := []struct {
		secs int
		want time.Duration
	}{
		{0, defaultTimeoutSeconds * time.Second},
		{1, 1 * time.Second},
		{maxTimeoutSeconds, maxTimeoutSeconds * time.Second},
		{maxTimeoutSeconds + 1, maxTimeoutSeconds * time.Second},
		{1 << 62, maxTimeoutSeconds * time.Second}, // would overflow if unbounded
	}
	for _, tc := range cases {
		got := timeoutFromSeconds(tc.secs)
		if got != tc.want {
			t.Errorf("timeoutFromSeconds(%d) = %s, want %s", tc.secs, got, tc.want)
		}
		if got <= 0 {
			t.Errorf("timeoutFromSeconds(%d) = %s, must be positive", tc.secs, got)
		}
	}
}

// TestConfig_StringRedactsToken verifies formatting a Config never discloses the
// bearer token via %v, %+v, %s, or %#v (m-03).
func TestConfig_StringRedactsToken(t *testing.T) {
	secret := "super-secret-jwt-value"
	c := Config{Enabled: true, Endpoint: "https://argocd.example.com", token: secret}

	for _, verb := range []string{"%v", "%+v", "%s", "%#v"} {
		out := fmt.Sprintf(verb, c)
		if strings.Contains(out, secret) {
			t.Errorf("formatting Config with %q leaked the token: %s", verb, out)
		}
		if !strings.Contains(out, "<redacted>") {
			t.Errorf("formatting Config with %q should show a redaction placeholder, got: %s", verb, out)
		}
	}

	// An unset token renders as an empty string literal, never as <redacted>.
	empty := Config{Enabled: false}
	if strings.Contains(fmt.Sprintf("%v", empty), "<redacted>") {
		t.Error("an unset token must not render as <redacted>")
	}

	// The accessor still returns the real token for same-package consumers.
	if c.BearerToken() != secret {
		t.Errorf("BearerToken() = %q, want %q", c.BearerToken(), secret)
	}
}

// TestReadFileLimited_RejectsNonRegularFile verifies the Q-10 fail-closed file
// policy: a FIFO (a non-regular file whose blocking read could otherwise hang
// startup) is rejected with a descriptive error rather than hanging or being
// read. Opening with O_NONBLOCK ensures the open returns immediately; the
// opened-descriptor stat then rejects the non-regular target.
func TestReadFileLimited_RejectsNonRegularFile(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "argocd-token-fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Skipf("cannot create FIFO on this platform: %v", err)
	}

	_, err := readFileLimited(fifo, maxTokenFileBytes)
	if err == nil {
		t.Fatal("readFileLimited(FIFO) error = nil, want a non-regular-file rejection")
	}
	if !strings.Contains(err.Error(), "regular file") {
		t.Errorf("error = %q, want it to report that the file is not a regular file", err.Error())
	}
}

// TestReadFileLimited_AcceptsSymlinkToRegularFile verifies that a regular file
// reached through a symlink — the common Kubernetes Secret mount posture (the
// ..data symlink) — is still read successfully, so the Q-10 hardening does not
// break real deployments.
func TestReadFileLimited_AcceptsSymlinkToRegularFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real-token")
	if err := ioutil.WriteFile(target, []byte("tok-xyz"), 0600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "token-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink on this platform: %v", err)
	}

	data, err := readFileLimited(link, maxTokenFileBytes)
	if err != nil {
		t.Fatalf("readFileLimited(symlink->regular) error = %v, want nil", err)
	}
	if string(data) != "tok-xyz" {
		t.Errorf("data = %q, want %q", string(data), "tok-xyz")
	}
}

// TestConfigFromMonkey_NonRegularTokenFileRejected verifies the Q-10 policy end
// to end through the public config path: an enabled integration whose token_file
// is a FIFO fails closed with an error rather than hanging the terminate run.
func TestConfigFromMonkey_NonRegularTokenFileRejected(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "token-fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Skipf("cannot create FIFO on this platform: %v", err)
	}

	m := config.Defaults()
	m.Set(param.ArgoCDEnabled, true)
	m.Set(param.ArgoCDEndpoint, "https://argocd.example.com")
	m.Set(param.ArgoCDTokenFile, fifo)

	if _, err := configFromMonkey(m); err == nil {
		t.Error("configFromMonkey with a FIFO token_file error = nil, want a non-regular-file rejection")
	}
}

// TestSanitizeForLog verifies the Q-15 log-injection defense: control characters
// (notably CR/LF that could forge additional log lines) are escaped into a
// visible form, ordinary printable text is returned unchanged (so existing
// status/name/id log output and the reason-substring assertions that depend on
// it are preserved), and an oversized value is capped.
func TestSanitizeForLog(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain status unchanged", "OutOfSync", "OutOfSync"},
		{"app name unchanged", "my-app-123", "my-app-123"},
		{"newline escaped", "line1\nnot terminating: FORGED", `line1\nnot terminating: FORGED`},
		{"carriage return escaped", "a\rb", `a\rb`},
		{"tab escaped", "a\tb", `a\tb`},
		{"nul escaped", "a\x00b", `a\x00b`},
		{"del escaped", "a\x7fb", `a\x7fb`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeForLog(tc.in); got != tc.want {
				t.Errorf("sanitizeForLog(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// A value longer than the cap is truncated with a visible marker and never
	// grows unbounded.
	long := strings.Repeat("x", maxLogFieldRunes+50)
	got := sanitizeForLog(long)
	if !strings.HasSuffix(got, "...(truncated)") {
		t.Error("oversized value was not truncated with the expected marker")
	}
	if len([]rune(got)) > maxLogFieldRunes+len("...(truncated)") {
		t.Errorf("truncated value too long: %d runes", len([]rune(got)))
	}
}
