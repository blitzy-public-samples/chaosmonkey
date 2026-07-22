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
	"io/ioutil"
	"strings"
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
	// deadline-disabling value. One hour is far beyond any reasonable per-request
	// Argo CD API timeout. (Overflow bound added per code review m-02.)
	maxTimeoutSeconds = 3600
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

	// A token file, when provided, takes precedence over the inline token.
	if c.TokenFile != "" {
		raw, rerr := ioutil.ReadFile(c.TokenFile)
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
