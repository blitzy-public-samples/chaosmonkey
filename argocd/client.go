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
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/pkg/errors"
)

// Size caps for the Argo CD client. They bound untrusted or accidentally
// oversized inputs so the integration fails closed instead of exhausting
// memory. (Bounded reads added for the Argo CD integration per code review M-04.)
const (
	// maxCACertBytes bounds the CA bundle file read in NewClient.
	maxCACertBytes = 1 << 20 // 1 MiB
	// maxApplicationResponseBytes bounds a successful GET Application body before
	// it is decoded, so a malicious or runaway response cannot exhaust memory.
	maxApplicationResponseBytes = 8 << 20 // 8 MiB
	// maxManagedResources caps the number of status.resources[] entries accepted
	// from a decoded Application; a response exceeding it is rejected (fail-closed)
	// rather than driving unbounded downstream work.
	maxManagedResources = 10000
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

// String returns a credential-redacted representation of the Client so that
// logging it (with %v, %+v, or %s) can never disclose the bearer token. The
// token's presence is shown only as a fixed placeholder, mirroring the
// redaction already applied to Config. (Added for the Argo CD integration per
// code review m-02.)
func (cl *Client) String() string {
	return fmt.Sprintf("argocd.Client{endpoint:%q project:%q token:%s}",
		cl.endpoint, cl.project, redactedToken(cl.token))
}

// GoString returns a credential-redacted Go-syntax representation so that %#v
// also never discloses the bearer token. (Added for the Argo CD integration per
// code review m-02.)
func (cl *Client) GoString() string { return cl.String() }

// NewClient builds an Argo CD REST client from a Config. TLS is configurable:
// a CA bundle (RootCAs) when Config.CACert is set, otherwise optional
// InsecureSkipVerify. The endpoint's trailing slash is trimmed. Returns an
// error only for genuine construction problems (missing endpoint, unreadable
// or invalid CA bundle).
func NewClient(c Config) (*Client, error) {
	if c.Endpoint == "" {
		return nil, errors.New("argocd: no endpoint configured")
	}

	// Validate the endpoint before any request is built. This closes the
	// credential-leak vectors from code review C-06: it rejects a non-absolute
	// or non-http(s) URL, a URL that embeds credentials (userinfo), and any
	// non-loopback host that would carry the bearer token over cleartext http.
	if err := validateEndpoint(c.Endpoint); err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{}
	switch {
	case c.CACert != "":
		// Bounded read so an oversized/adversarial CA file fails closed instead
		// of exhausting memory (code review M-04).
		pemData, err := readFileLimited(c.CACert, maxCACertBytes)
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

	// Clone the standard-library default transport so the client keeps its
	// production defaults — proxy selection from the environment
	// (HTTP(S)_PROXY / NO_PROXY via http.ProxyFromEnvironment), connection dial
	// timeouts, keep-alives, and idle-connection tuning — instead of a bare
	// zero-value transport that silently drops all of them and would fail closed
	// behind an enterprise egress proxy. Only the TLS configuration is
	// overridden. (Transport hardening added per code review Q-11.)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	// Keep HTTP/2 disabled on this client. The integration performs only simple
	// GET/PATCH calls for which HTTP/1.1 is sufficient, and disabling HTTP/2
	// keeps the client off the HTTP/2-specific TLS attack surface flagged for the
	// pinned runtime (code review Q-11 / Q-17) without requiring a toolchain
	// change. A non-nil, empty TLSNextProto disables the HTTP/2 upgrade even
	// though a custom TLSClientConfig is set.
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = make(map[string]func(authority string, c *tls.Conn) http.RoundTripper)
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   c.Timeout,
		// Refuse to follow redirects. Following a redirect can forward the
		// bearer token to a different host and, on the affected Go runtime
		// (GO-2025-3420 / CVE-2024-45336), is subject to sensitive-header
		// leakage across a redirect chain. Returning an error stops the chain
		// before any header is re-sent, so the client-side protection holds
		// regardless of the runtime patch level (code review C-06).
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return errors.Errorf("argocd: refusing to follow redirect to %s (possible credential exposure)", req.URL.Redacted())
		},
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

// validateEndpoint parses and validates the configured Argo CD endpoint before
// any request is built, closing the credential-leak vectors from code review
// C-06. It requires an absolute http/https URL with a host, rejects a URL that
// embeds credentials (userinfo), and requires HTTPS for any non-loopback host so
// a bearer token is never transmitted in cleartext to a remote server. Plain
// http is permitted ONLY for loopback hosts (localhost / 127.0.0.1 / ::1), which
// supports local development and in-process test servers without weakening
// production security.
func validateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.Wrapf(err, "argocd: invalid endpoint %q", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.Errorf("argocd: endpoint %q must use http or https", raw)
	}
	// Reject an opaque URL (e.g. "https:foo") before the host check so it yields
	// a precise diagnostic. An opaque URL has no authority component, so joining
	// it with the REST path would produce a nonsensical request target.
	// (Endpoint hardening added per code review Q-14.)
	if u.Opaque != "" {
		return errors.Errorf("argocd: endpoint %q must be a normal absolute URL, not an opaque one", raw)
	}
	if u.Host == "" {
		return errors.Errorf("argocd: endpoint %q is missing a host", raw)
	}
	if u.User != nil {
		return errors.New("argocd: endpoint must not embed credentials (userinfo)")
	}
	// Reject a query string or a fragment. The endpoint is string-joined with the
	// REST path (.../api/v1/applications/{name}); a query or fragment carried on
	// the base URL would corrupt every constructed request target (and query
	// content could leak into logs), so it is rejected up front. A normal path is
	// intentionally still allowed so a base-path deployment (e.g. behind an
	// ingress at https://host/argocd) continues to work with the existing path
	// concatenation. (Endpoint hardening added per code review Q-14.)
	if u.RawQuery != "" || u.ForceQuery {
		return errors.Errorf("argocd: endpoint %q must not contain a query string", raw)
	}
	if u.Fragment != "" {
		return errors.Errorf("argocd: endpoint %q must not contain a URL fragment", raw)
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return errors.Errorf("argocd: endpoint %q must use https (plain http is allowed only for loopback hosts)", raw)
	}
	return nil
}

// isLoopbackHost reports whether host is a loopback name or address, for which
// plain http is tolerated (local development / in-process test servers).
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// applicationURL builds the REST URL for a single Application GET, adding the
// optional project query parameter when a project scope is configured.
func (cl *Client) applicationURL(name string) string {
	u := fmt.Sprintf("%s/api/v1/applications/%s", cl.endpoint, url.PathEscape(name))
	if cl.project != "" {
		u += "?project=" + url.QueryEscape(cl.project)
	}
	return u
}

// patchURL builds the REST URL for the ApplicationService.Patch endpoint. Unlike
// applicationURL it does NOT append a project query parameter: for a PATCH the
// project scope travels inside the ApplicationPatchRequest body, matching the
// official contract (code review C-01/M-03).
func (cl *Client) patchURL(name string) string {
	return fmt.Sprintf("%s/api/v1/applications/%s", cl.endpoint, url.PathEscape(name))
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

	// Read the body under a hard size cap so a malicious or runaway response
	// fails closed instead of exhausting memory. Reading one byte past the cap
	// lets us distinguish a legitimate body from an over-limit one (code review
	// M-04).
	body, err := ioutil.ReadAll(io.LimitReader(resp.Body, maxApplicationResponseBytes+1))
	if err != nil {
		return nil, errors.Wrapf(err, "argocd: failed to read response body from %s", u)
	}
	if int64(len(body)) > maxApplicationResponseBytes {
		return nil, errors.Errorf("argocd: application %q response exceeds %d-byte limit", name, maxApplicationResponseBytes)
	}

	var out Application
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, errors.Wrapf(err, "argocd: failed to decode application %q", name)
	}
	// Reject a pathological resource inventory (fail-closed) so an absurd
	// status.resources[] cannot drive unbounded downstream work (code review M-04).
	if len(out.Status.Resources) > maxManagedResources {
		return nil, errors.Errorf("argocd: application %q reports %d resources, exceeding the %d limit",
			name, len(out.Status.Resources), maxManagedResources)
	}
	return &out, nil
}

// mergePatch is the INNER Kubernetes JSON merge patch that sets a single
// annotation on an Application's metadata. It is serialized to a JSON string and
// carried in the ApplicationPatchRequest envelope's "patch" field (it is NOT the
// HTTP body on its own).
type mergePatch struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
}

// applicationPatchRequest is the Argo CD ApplicationService.Patch request
// envelope for PATCH /api/v1/applications/{name}. It mirrors the official
// argoproj.io ApplicationPatchRequest message: the inner Kubernetes merge patch
// travels as a JSON *string* in Patch, PatchType selects the merge strategy
// ("merge"), and Project (when set) scopes the write inside the body rather than
// as a query parameter. Sending this envelope with Content-Type application/json
// is what the Argo CD API actually accepts — the previous raw merge-patch body
// was rejected by the server. (Envelope added per code review C-01/M-03.)
type applicationPatchRequest struct {
	Name      string `json:"name"`
	Patch     string `json:"patch"`
	PatchType string `json:"patchType"`
	Project   string `json:"project,omitempty"`
}

// patchTypeMerge is the ApplicationService.Patch strategy for a JSON merge
// patch. The whole-Application Patch endpoint accepts "json" or "merge"; the
// annotation write-back always uses a merge patch.
const patchTypeMerge = "merge"

// PatchApplicationAnnotation sets a single annotation (key=value) on the
// Application's metadata via the Argo CD ApplicationService.Patch endpoint. It
// is annotation-only: it does not trigger a sync, refresh, or rollback. Callers
// (the tracker) treat any error as non-fatal and swallow it. A non-2xx response
// yields an error.
//
// The inner {"metadata":{"annotations":{key:value}}} merge patch is wrapped in
// the official ApplicationPatchRequest envelope (name + patch-as-JSON-string +
// patchType "merge" + optional project) and sent as application/json, matching
// the Argo CD REST contract (code review C-01/M-03).
//
// Argo CD Notifications subscription annotations (of the form
// notifications.argoproj.io/subscribe.<trigger>.<service>) are an optional
// future enhancement layered on this same annotation mechanism; they are
// intentionally not implemented here.
func (cl *Client) PatchApplicationAnnotation(ctx context.Context, name, key, value string) (err error) {
	var patch mergePatch
	patch.Metadata.Annotations = map[string]string{key: value}

	inner, err := json.Marshal(patch)
	if err != nil {
		return errors.Wrap(err, "argocd: could not marshal annotation patch")
	}

	// Wrap the inner merge patch in the official Argo CD request envelope. The
	// merge patch is a JSON string in "patch"; the project scope (if any) is in
	// the body, not the query string (code review C-01/M-03).
	payload, err := json.Marshal(applicationPatchRequest{
		Name:      name,
		Patch:     string(inner),
		PatchType: patchTypeMerge,
		Project:   cl.project,
	})
	if err != nil {
		return errors.Wrap(err, "argocd: could not marshal application patch request")
	}

	u := cl.patchURL(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, bytes.NewReader(payload))
	if err != nil {
		return errors.Wrapf(err, "argocd: could not build patch request for %s", u)
	}
	req.Header.Set("Authorization", "Bearer "+cl.token)
	req.Header.Set("Content-Type", "application/json")

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

	// Defense-in-depth (code review C-04): confirm the PATCH acted on the
	// Application we targeted. Argo CD's Patch endpoint echoes the patched
	// Application; a metadata.name that differs from the requested name means the
	// write was routed to a different Application (a rename or misroute between
	// the resolving GET and this PATCH), so reject it. The configured project is
	// already enforced server-side (a wrong project yields 403 above). The body
	// is read under the same hard size cap as GET so a runaway response fails
	// closed (M-04). An absent or non-JSON 2xx body is tolerated — the status
	// already confirms the write — so best-effort write-back is not defeated by a
	// server that returns a minimal body.
	body, err := ioutil.ReadAll(io.LimitReader(resp.Body, maxApplicationResponseBytes+1))
	if err != nil {
		return errors.Wrapf(err, "argocd: failed to read patch response body from %s", u)
	}
	if int64(len(body)) > maxApplicationResponseBytes {
		return errors.Errorf("argocd: patch response for %q exceeds %d-byte limit", name, maxApplicationResponseBytes)
	}
	var patched Application
	if err := json.Unmarshal(body, &patched); err != nil {
		return nil
	}
	if got := strings.TrimSpace(patched.Metadata.Name); got != "" && got != name {
		return errors.Errorf("argocd: annotation patch for %q was applied to a different application %q; refusing to trust write-back", name, got)
	}
	return nil
}
