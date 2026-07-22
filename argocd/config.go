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
// (b) writes a best-effort annotation back to the Application via a Tracker so
// chaos actions appear in the Argo CD timeline. It is inert when unconfigured
// and is implemented entirely with the Go standard library (net/http,
// encoding/json, crypto/tls) plus github.com/pkg/errors, adding no new
// third-party dependencies.
package argocd

import (
	"io/ioutil"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/Netflix/chaosmonkey/v2/config"
)

// Config is a typed, immutable snapshot of the optional [argocd] TOML section.
// It is produced by configFromMonkey and consumed by NewClient/NewMapper. When
// the feature is disabled, callers do not build a Config at all (GetPrecheck
// short-circuits to an allow-all provider).
type Config struct {
	Enabled            bool          // argocd.enabled
	Endpoint           string        // argocd.endpoint (base URL, no trailing slash required)
	Token              string        // resolved bearer token (from TokenFile if set, else inline)
	TokenFile          string        // argocd.token_file (path; takes precedence over Token)
	Project            string        // argocd.project (optional lookup scope)
	Applications       []string      // argocd.applications (chaos-eligible Application names/selectors)
	InsecureSkipVerify bool          // argocd.insecure_skip_verify
	CACert             string        // argocd.ca_cert (path to PEM CA bundle)
	Timeout            time.Duration // derived from argocd.timeout seconds (default 30s)
}

// defaultTimeoutSeconds is used when argocd.timeout is unset or non-positive.
const defaultTimeoutSeconds = 30

// configFromMonkey builds a Config from the Chaos Monkey configuration by
// reading the typed [argocd] accessors. It applies the token-file precedence
// rule (a non-empty token_file overrides the inline token) and converts the
// timeout from seconds to a time.Duration (defaulting to 30s when unset/<=0).
func configFromMonkey(cfg *config.Monkey) (Config, error) {
	apps, err := cfg.ArgoCDApplications()
	if err != nil {
		return Config{}, errors.Wrap(err, "argocd: could not read argocd.applications")
	}

	c := Config{
		Enabled:            cfg.ArgoCDEnabled(),
		Endpoint:           cfg.ArgoCDEndpoint(),
		Token:              cfg.ArgoCDToken(),
		TokenFile:          cfg.ArgoCDTokenFile(),
		Project:            cfg.ArgoCDProject(),
		Applications:       apps,
		InsecureSkipVerify: cfg.ArgoCDInsecureSkipVerify(),
		CACert:             cfg.ArgoCDCACert(),
	}

	// Convert seconds -> duration, defaulting when unset or non-positive.
	secs := cfg.ArgoCDTimeout()
	if secs <= 0 {
		secs = defaultTimeoutSeconds
	}
	c.Timeout = time.Duration(secs) * time.Second

	// A token file, when provided, takes precedence over the inline token.
	if c.TokenFile != "" {
		raw, rerr := ioutil.ReadFile(c.TokenFile)
		if rerr != nil {
			return Config{}, errors.Wrapf(rerr, "argocd: could not read token_file %q", c.TokenFile)
		}
		c.Token = strings.TrimSpace(string(raw))
	}

	return c, nil
}
