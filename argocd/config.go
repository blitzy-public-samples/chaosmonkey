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

// Package argocd provides an optional, opt-in, client-side integration between
// Chaos Monkey and Argo CD (GitOps). When enabled it (a) gates terminations on
// the governing Argo CD Application's sync/health state via a Precheck, and
// (b) writes a best-effort annotation onto the governing Application via a
// Tracker, recording the chaos action in the Application's annotations. That
// annotation is visible through the Argo CD API and the Application's
// resource/manifest view in the UI; surfacing it as a notification additionally
// requires a custom Argo CD Notifications trigger and template. The integration
// is inert when unconfigured and is implemented entirely with the Go standard
// library (net/http, encoding/json, crypto/tls) plus github.com/pkg/errors,
// adding no new third-party dependencies.
package argocd

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/pkg/errors"

	"github.com/Netflix/chaosmonkey/v2/config"
)

// Config is a typed snapshot of the optional [argocd] TOML section. It is
// produced by configFromMonkey and consumed by the same-package client/mapper.
// The bearer token is held in the unexported token field and is never included
// in the String/GoString representations, so a Config can be logged without
// disclosing the credential. Callers outside the package cannot read or mutate
// the token. (Token encapsulation added per code review m-03.)
type Config struct {
	Enabled            bool          // argocd.enabled
	Endpoint           string        // argocd.endpoint (base URL, no trailing slash required)
	token              string        // resolved bearer token (from TokenFile if set, else inline); redacted in String
	TokenFile          string        // argocd.token_file (path; takes precedence over the inline token)
	Project            string        // argocd.project (optional lookup scope)
	Applications       []string      // argocd.applications (exact chaos-eligible Application names, normalized)
	InsecureSkipVerify bool          // argocd.insecure_skip_verify
	CACert             string        // argocd.ca_cert (path to PEM CA bundle)
	Timeout            time.Duration // derived from argocd.timeout seconds (default 30s, bounded)
}

const (
	// defaultTimeoutSeconds is used when argocd.timeout is unset or non-positive.
	defaultTimeoutSeconds = 30
	// maxTimeoutSeconds bounds argocd.timeout so converting to a time.Duration
	// (multiplying by time.Second) cannot overflow int64 into a non-positive,
	// deadline-disabling value. One hour is far beyond any reasonable
	// single-operation Argo CD API timeout. (Overflow bound added per code review m-02.)
	maxTimeoutSeconds = 3600
	// maxTokenFileBytes bounds the token_file read. A bearer JWT / project-role
	// token is far smaller than this; the cap prevents an oversized or
	// adversarial file from exhausting memory, matching the fail-closed posture
	// used elsewhere. (Bounded read added per code review M-04.)
	maxTokenFileBytes = 1 << 16 // 64 KiB
)

// BearerToken returns the resolved bearer token for same-package callers (the
// client). It is a method rather than an exported field so the credential is
// not accidentally emitted by struct formatting and cannot be mutated by
// callers. (Added for the Argo CD integration per code review m-03.)
func (c Config) BearerToken() string { return c.token }

// String returns a human-readable, credential-redacted representation of the
// Config. The bearer token is never included, so accidentally logging a Config
// (with %v, %+v, or %s) cannot disclose the credential.
// (Redaction added per code review m-03.)
func (c Config) String() string {
	return fmt.Sprintf("argocd.Config{Enabled:%t Endpoint:%q Token:%s TokenFile:%q Project:%q Applications:%v InsecureSkipVerify:%t CACert:%q Timeout:%s}",
		c.Enabled, c.Endpoint, redactedToken(c.token), c.TokenFile, c.Project, c.Applications, c.InsecureSkipVerify, c.CACert, c.Timeout)
}

// GoString returns a credential-redacted Go-syntax representation so that %#v
// also never discloses the bearer token. (Redaction added per code review m-03.)
func (c Config) GoString() string { return c.String() }

// redactedToken renders a sensitive value as a fixed placeholder when set, or
// an empty string literal when unset, so its presence (never its content) shows.
func redactedToken(s string) string {
	if s == "" {
		return `""`
	}
	return `"<redacted>"`
}

// maxLogFieldRunes caps the length of a single externally-influenced value
// embedded in a log line, so an oversized value cannot flood the scheduler log.
const maxLogFieldRunes = 256

// sanitizeForLog makes an externally-influenced string safe to embed in a single
// log line. Argo CD status strings, Application names, and instance identifiers
// can in principle contain newlines or other control characters; interpolating
// them raw would let a crafted value forge additional, misleading log entries
// (log injection, CWE-117). sanitizeForLog escapes CR/LF/TAB and any other
// control character (below 0x20, or DEL) into a visible \xNN form and caps the
// result length. It is the identity function for ordinary printable text, so it
// does not alter normal status/name/id output already relied upon by callers and
// tests. (Added for the Argo CD integration per code review Q-15.)
func sanitizeForLog(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= maxLogFieldRunes {
			b.WriteString("...(truncated)")
			break
		}
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
		n++
	}
	return b.String()
}

// readFileLimited reads at most max bytes from the file at path and returns an
// explicit overflow error if the file is larger, so an oversized or adversarial
// file fails closed instead of exhausting memory. It is shared by NewClient (CA
// bundle) and configFromMonkey (token file). The file handle is closed with the
// same named-return pattern used elsewhere in the repository so a close error is
// not silently discarded. (Bounded read added per code review M-04.)
func readFileLimited(path string, max int64) (data []byte, err error) {
	// Open with O_NONBLOCK so the open itself cannot block on a FIFO/device whose
	// reader side would otherwise wait indefinitely for a writer. A misconfigured
	// or adversarial token/CA path must fail fast rather than hang the terminate
	// run before any HTTP deadline can apply (code review Q-10). O_NONBLOCK has no
	// effect on the subsequent reads of a regular file, which is all this
	// function ever proceeds to read.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = errors.Wrapf(cerr, "argocd: closing %q", path)
		}
	}()

	// Validate the OPENED descriptor before reading (code review Q-10). Stat the
	// handle we actually hold — not a pre-open Lstat, which would be a TOCTOU
	// race between the check and the read — and reject anything that is not a
	// regular file. This fails closed on a FIFO/device/socket (which O_NONBLOCK
	// let us open without hanging) while still accepting a regular file reached
	// through a symlink (e.g. a Kubernetes Secret mounted via the ..data
	// symlink), which is the common, supported production posture for the token
	// and CA files.
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Wrapf(err, "argocd: could not stat %q", path)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.Errorf("argocd: %q is not a regular file (mode %s); refusing to read a non-regular credential/CA file", path, info.Mode())
	}

	// Read one byte beyond the cap so an exactly-at-limit file is accepted while
	// anything larger is detected and rejected.
	data, err = ioutil.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, errors.Wrapf(err, "argocd: reading %q", path)
	}
	if int64(len(data)) > max {
		return nil, errors.Errorf("argocd: file %q exceeds the %d-byte limit", path, max)
	}
	return data, nil
}

// warnIfTokenFileInsecure emits a best-effort, non-fatal warning when the
// resolved token file is group/world-accessible, so an operator is alerted to a
// credential-exposure risk without breaking a working deployment. The policy is
// advisory (logged, not enforced) because the token file's ownership/permission
// model is deployment-specific — for example a Kubernetes Secret is commonly
// mounted world-readable inside the container — and the recommended, documented
// posture is an owner-only (0600) file.
//
// It uses Stat (following a symlink to its target) so the check reflects the
// file that will actually be read; a symlink to a regular file (e.g. a
// Kubernetes Secret mounted through the ..data symlink) is a normal, supported
// posture and is intentionally NOT flagged as suspicious here. A genuinely
// non-regular target (FIFO/device/socket) is separately and firmly rejected by
// readFileLimited, which validates the opened descriptor. (Refined per code
// review Q-10; originally added per code review m-02; documented in
// docs/plugins/ArgoCD.md.)
func warnIfTokenFileInsecure(path string) {
	info, err := os.Stat(path)
	if err != nil {
		// A stat error here is not authoritative; the subsequent bounded read
		// surfaces any genuine read failure as a wrapped error.
		return
	}
	mode := info.Mode()
	if mode.IsRegular() && mode.Perm()&0o077 != 0 {
		log.Printf("argocd: WARNING: token_file %q is group/world-accessible (mode %#o); restrict it to owner-only (0600)", path, mode.Perm())
	}
}

// timeoutFromSeconds converts a configured timeout in seconds to a
// time.Duration, defaulting when non-positive and clamping to an upper bound so
// the multiplication by time.Second cannot overflow int64 into a non-positive
// (deadline-disabling) duration. (Overflow bound added per code review m-02.)
func timeoutFromSeconds(secs int) time.Duration {
	switch {
	case secs <= 0:
		secs = defaultTimeoutSeconds
	case secs > maxTimeoutSeconds:
		secs = maxTimeoutSeconds
	}
	return time.Duration(secs) * time.Second
}

// configFromMonkey builds a Config from the Chaos Monkey configuration by
// reading the typed [argocd] accessors.
//
// It honors the disabled state FIRST: when argocd.enabled is false it returns
// an inert, disabled snapshot without reading the token file, applications, or
// any other setting, preserving opt-in inertness and avoiding credential-file
// I/O for a disabled feature (per code review M-03). When enabled it applies the
// token-file precedence rule (a non-empty token_file overrides the inline
// token), trims both token sources, rejects an empty resolved credential so an
// empty bearer header is never formed (per code review m-01), normalizes the
// eligible Application names, and converts the timeout from seconds to a bounded
// time.Duration.
func configFromMonkey(cfg *config.Monkey) (Config, error) {
	// Nil-config guard: dereferencing the accessors on a nil *config.Monkey would
	// panic. Fail closed with a descriptive error instead so a mis-wired caller
	// results in the feature being unavailable rather than crashing the
	// scheduler. (Nil guard added per code review M-04.)
	if cfg == nil {
		return Config{}, errors.New("argocd: nil configuration")
	}

	// Disabled short-circuit: no file/credential/application work when off.
	if !cfg.ArgoCDEnabled() {
		return Config{Enabled: false}, nil
	}

	apps, err := cfg.ArgoCDApplications()
	if err != nil {
		return Config{}, errors.Wrap(err, "argocd: could not read argocd.applications")
	}

	c := Config{
		Enabled:            true,
		Endpoint:           strings.TrimSpace(cfg.ArgoCDEndpoint()),
		token:              strings.TrimSpace(cfg.ArgoCDToken()),
		TokenFile:          strings.TrimSpace(cfg.ArgoCDTokenFile()),
		Project:            cfg.ArgoCDProject(),
		Applications:       normalizeApplications(apps),
		InsecureSkipVerify: cfg.ArgoCDInsecureSkipVerify(),
		CACert:             cfg.ArgoCDCACert(),
		Timeout:            timeoutFromSeconds(cfg.ArgoCDTimeout()),
	}

	// A token file, when provided, takes precedence over the inline token. The
	// read is bounded (fail-closed on an oversized file) and preceded by an
	// advisory permission check. (Bounded read + permission policy added per
	// code review M-04 and m-02.)
	if c.TokenFile != "" {
		warnIfTokenFileInsecure(c.TokenFile)
		raw, rerr := readFileLimited(c.TokenFile, maxTokenFileBytes)
		if rerr != nil {
			return Config{}, errors.Wrapf(rerr, "argocd: could not read token_file %q", c.TokenFile)
		}
		c.token = strings.TrimSpace(string(raw))
	}

	// When enabled, a bearer credential is mandatory: fail closed rather than
	// forming a request with an empty Authorization header.
	if c.token == "" {
		return Config{}, errors.New("argocd: enabled but no bearer token resolved (set argocd.token or argocd.token_file)")
	}

	return c, nil
}
