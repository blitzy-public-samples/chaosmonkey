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

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"

	"github.com/pkg/errors"
)

// Client is a minimal Argo CD REST API client built on the Go standard library.
// It performs read-only Application status queries and a best-effort annotation
// write; it never triggers syncs, refreshes, or rollbacks.
type Client struct {
	endpoint   string       // base URL, trailing slash trimmed
	token      string       // bearer token (JWT / project-role token)
	project    string       // optional project scope for lookups
	httpClient *http.Client // configured with TLS + timeout
}

// NewClient builds an Argo CD REST client from a Config. TLS is configurable:
// a CA bundle (RootCAs) when Config.CACert is set, otherwise optional
// InsecureSkipVerify. The endpoint's trailing slash is trimmed. Returns an
// error only for genuine construction problems (missing endpoint, unreadable
// or invalid CA bundle).
func NewClient(c Config) (*Client, error) {
	if c.Endpoint == "" {
		return nil, errors.New("argocd: no endpoint configured")
	}

	tlsConfig := &tls.Config{}
	switch {
	case c.CACert != "":
		pemData, err := ioutil.ReadFile(c.CACert)
		if err != nil {
			return nil, errors.Wrapf(err, "argocd: could not read ca_cert %q", c.CACert)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemData) {
			return nil, errors.Errorf("argocd: no valid PEM certificates found in ca_cert %q", c.CACert)
		}
		tlsConfig.RootCAs = pool
	case c.InsecureSkipVerify:
		tlsConfig.InsecureSkipVerify = true
	}

	transport := &http.Transport{TLSClientConfig: tlsConfig}
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   c.Timeout,
	}

	return &Client{
		endpoint: strings.TrimRight(c.Endpoint, "/"),
		// Argo CD integration: the bearer token is encapsulated on Config
		// (unexported field) and exposed to same-package callers via the
		// BearerToken accessor so it is never disclosed by struct formatting.
		token:      c.BearerToken(),
		project:    c.Project,
		httpClient: httpClient,
	}, nil
}

// applicationURL builds the REST URL for a single Application, adding the
// optional project query parameter when a project scope is configured.
func (cl *Client) applicationURL(name string) string {
	u := fmt.Sprintf("%s/api/v1/applications/%s", cl.endpoint, url.PathEscape(name))
	if cl.project != "" {
		u += "?project=" + url.QueryEscape(cl.project)
	}
	return u
}

// GetApplication fetches a single Argo CD Application by name. It attaches the
// bearer token, honors the context deadline, and maps HTTP status codes to
// descriptive errors: 404 (missing), 403 (forbidden / wrong project), any
// other non-200 (unexpected status). On success it decodes the JSON body into
// an *Application.
func (cl *Client) GetApplication(ctx context.Context, name string) (app *Application, err error) {
	u := cl.applicationURL(name)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errors.Wrapf(err, "argocd: could not build request for %s", u)
	}
	req.Header.Set("Authorization", "Bearer "+cl.token)

	resp, err := cl.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrapf(err, "argocd: request to %s failed", u)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			err = errors.Wrapf(cerr, "argocd: failed to close response body from %s", u)
		}
	}()

	switch resp.StatusCode {
	case http.StatusOK:
		// fall through to decode
	case http.StatusNotFound:
		return nil, errors.Errorf("argocd: application %q not found (404)", name)
	case http.StatusForbidden:
		return nil, errors.Errorf("argocd: access to application %q forbidden (403); check token/project", name)
	default:
		return nil, errors.Errorf("argocd: unexpected status %d fetching application %q", resp.StatusCode, name)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrapf(err, "argocd: failed to read response body from %s", u)
	}

	var out Application
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, errors.Wrapf(err, "argocd: failed to decode application %q", name)
	}
	return &out, nil
}

// mergePatch is the body of a JSON merge patch that sets a single annotation on
// an Application's metadata.
type mergePatch struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
}

// PatchApplicationAnnotation applies a JSON merge patch that sets a single
// annotation (key=value) on the Application's metadata. It is annotation-only:
// it does not trigger a sync, refresh, or rollback. Callers (the tracker) treat
// any error as non-fatal and swallow it. A non-2xx response yields an error.
//
// Argo CD Notifications subscription annotations (of the form
// notifications.argoproj.io/subscribe.<trigger>.<service>) are an optional
// future enhancement layered on this same annotation mechanism; they are
// intentionally not implemented here.
func (cl *Client) PatchApplicationAnnotation(ctx context.Context, name, key, value string) (err error) {
	var patch mergePatch
	patch.Metadata.Annotations = map[string]string{key: value}

	payload, err := json.Marshal(patch)
	if err != nil {
		return errors.Wrap(err, "argocd: could not marshal annotation patch")
	}

	u := cl.applicationURL(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, bytes.NewReader(payload))
	if err != nil {
		return errors.Wrapf(err, "argocd: could not build patch request for %s", u)
	}
	req.Header.Set("Authorization", "Bearer "+cl.token)
	req.Header.Set("Content-Type", "application/merge-patch+json")

	resp, err := cl.httpClient.Do(req)
	if err != nil {
		return errors.Wrapf(err, "argocd: patch request to %s failed", u)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			err = errors.Wrapf(cerr, "argocd: failed to close response body from %s", u)
		}
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.Errorf("argocd: annotation patch to %s returned status %d", u, resp.StatusCode)
	}
	return nil
}
