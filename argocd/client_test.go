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

// White-box unit tests for client.go. They exercise the Argo CD REST client
// against an in-process mock API built with net/http/httptest, covering:
//
//   - GetApplication: HTTP method, request path, bearer authentication header,
//     optional project query parameter, successful JSON decode, and the
//     descriptive 404 / 403 / non-2xx error mappings that the sync/health gate
//     later surfaces as human-readable skip reasons;
//   - NewClient: rejection of an empty endpoint, plus the configurable TLS
//     wiring (InsecureSkipVerify accepts httptest's self-signed certificate
//     while the default strict client rejects it);
//   - PatchApplicationAnnotation: the PATCH method, the application/json content
//     type, the bearer header, and the ApplicationPatchRequest envelope shape
//     (name + patch-as-JSON-string + patchType "merge" + optional project) whose
//     inner merge patch sets exactly the requested annotation, plus the non-2xx
//     error path (which the tracker swallows in production);
//   - endpoint hardening and resource bounds: rejection of a plaintext-http
//     remote endpoint and of an endpoint that embeds credentials, refusal to
//     follow redirects, a custom CA trust bundle, and the fail-closed
//     oversized-response guard.
//
// The tests use only the standard-library testing package with a table-driven
// style, matching every other test in this module (no test file imports
// testify). testify v1.8.1 is listed in go.mod only as an indirect requirement
// and its own required modules (e.g. pmezard/go-difflib, gopkg.in/yaml.v3) are
// absent, so importing it directly would make `go mod tidy` add those modules
// and promote testify to a direct require — a go.mod/go.sum change the
// minimal-change contract (AAP 0.3.2) forbids.

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// testApplicationJSON returns a minimal Argo CD Application document that
// decodes into an Application reporting an aggregate Synced/Healthy status and a
// single live Deployment workload. The exact JSON keys mirror the struct tags
// declared in application.go.
func testApplicationJSON() string {
	return `{"metadata":{"name":"my-app","namespace":"argocd","annotations":{}},` +
		`"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"},` +
		`"resources":[{"group":"apps","version":"v1","kind":"Deployment",` +
		`"namespace":"prod","name":"web","status":"Synced",` +
		`"health":{"status":"Healthy"}}]}}`
}

// newTestContext returns a context bounded by a short deadline so a stuck mock
// server can never hang the suite. The cancel function is registered with
// t.Cleanup, so callers need no explicit defer.
func newTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// writeBody writes a response body from an httptest handler. The (int, error)
// result of ResponseWriter.Write is intentionally discarded: an httptest
// in-memory writer does not fail, and discarding it explicitly keeps the file
// errcheck-clean without importing io.
func writeBody(w http.ResponseWriter, body string) {
	_, _ = w.Write([]byte(body))
}

// contains reports whether sub occurs within s. It is a tiny local helper so
// the test file needs no import beyond the whitelisted standard-library set;
// the strings package is intentionally avoided to keep the import surface
// minimal, consistent with the integration's dependency-conservatism rule.
func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// assertBearerToken verifies that the request carries exactly "Bearer <want>"
// in its Authorization header. On mismatch it reports a CREDENTIAL-SAFE message
// that never prints the header value or the expected token, only their byte
// lengths, so a failing assertion cannot disclose a secret to test logs.
// (Credential-safe assertions added per code review m-02.)
func assertBearerToken(t *testing.T, r *http.Request, want string) {
	t.Helper()
	got := r.Header.Get("Authorization")
	if got != "Bearer "+want {
		t.Errorf("Authorization header did not match the configured bearer token (got %d-byte value, want a %d-byte value)",
			len(got), len("Bearer "+want))
	}
}

// TestGetApplicationSuccess verifies the happy path: a GET to the correct
// Application path carrying the bearer token, a 200 response, and a body that
// decodes into a Synced/Healthy Application with its managed resources intact.
func TestGetApplicationSuccess(t *testing.T) {
	const wantToken = "test-token"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("request method = %q, want %q", r.Method, http.MethodGet)
		}
		if got, want := r.URL.Path, "/api/v1/applications/my-app"; got != want {
			t.Errorf("request path = %q, want %q", got, want)
		}
		assertBearerToken(t, r, wantToken)
		w.WriteHeader(http.StatusOK)
		writeBody(w, testApplicationJSON())
	}))
	defer srv.Close()

	cl, err := NewClient(Config{Endpoint: srv.URL, token: wantToken, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	app, err := cl.GetApplication(newTestContext(t), "my-app")
	if err != nil {
		t.Fatalf("GetApplication() error = %v", err)
	}
	if !app.IsSynced() {
		t.Error("app.IsSynced() = false, want true")
	}
	if !app.IsHealthy() {
		t.Error("app.IsHealthy() = false, want true")
	}
	if got, want := len(app.Status.Resources), 1; got != want {
		t.Errorf("len(Status.Resources) = %d, want %d", got, want)
	}
	if got, want := app.Metadata.Name, "my-app"; got != want {
		t.Errorf("Metadata.Name = %q, want %q", got, want)
	}
}

// TestGetApplicationSendsBearerHeader focuses on authentication: a distinct
// token value must be forwarded verbatim as an "Authorization: Bearer <token>"
// header, proving the credential flows through Config.BearerToken() and is not
// hard-coded anywhere in the client.
func TestGetApplicationSendsBearerHeader(t *testing.T) {
	const wantToken = "super-secret-jwt"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearerToken(t, r, wantToken)
		w.WriteHeader(http.StatusOK)
		writeBody(w, testApplicationJSON())
	}))
	defer srv.Close()

	cl, err := NewClient(Config{Endpoint: srv.URL, token: wantToken, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if _, err := cl.GetApplication(newTestContext(t), "my-app"); err != nil {
		t.Fatalf("GetApplication() error = %v", err)
	}
}

// TestGetApplicationSendsProjectQuery verifies that a configured project scope
// is propagated as the ?project= query parameter on the lookup request.
func TestGetApplicationSendsProjectQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Query().Get("project"), "myproj"; got != want {
			t.Errorf("project query = %q, want %q", got, want)
		}
		w.WriteHeader(http.StatusOK)
		writeBody(w, testApplicationJSON())
	}))
	defer srv.Close()

	cl, err := NewClient(Config{Endpoint: srv.URL, token: "t", Project: "myproj", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if _, err := cl.GetApplication(newTestContext(t), "my-app"); err != nil {
		t.Fatalf("GetApplication() error = %v", err)
	}
}

// TestGetApplicationErrors verifies that each non-200 status maps to a non-nil
// error and a nil Application. The descriptive substrings are asserted
// best-effort; the client surfaces these as the human-readable reasons the
// sync/health gate logs when it fails closed.
func TestGetApplicationErrors(t *testing.T) {
	tests := []struct {
		name            string
		status          int
		wantErrContains string
	}{
		{name: "not found", status: http.StatusNotFound, wantErrContains: "not found"},
		{name: "forbidden", status: http.StatusForbidden, wantErrContains: "forbidden"},
		{name: "unauthorized", status: http.StatusUnauthorized, wantErrContains: "status"},
		{name: "server error", status: http.StatusInternalServerError, wantErrContains: "status"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			cl, err := NewClient(Config{Endpoint: srv.URL, token: "t", Timeout: 5 * time.Second})
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}

			app, err := cl.GetApplication(newTestContext(t), "x")
			if err == nil {
				t.Fatalf("GetApplication() error = nil, want error for status %d", tc.status)
			}
			if app != nil {
				t.Errorf("GetApplication() app = %v, want nil on error", app)
			}
			if !contains(err.Error(), tc.wantErrContains) {
				t.Errorf("error = %q, want to contain %q", err.Error(), tc.wantErrContains)
			}
		})
	}
}

// TestNewClientInsecureTLS proves the configurable TLS wiring.
// httptest.NewTLSServer presents a self-signed certificate, so a client built
// with InsecureSkipVerify:true must succeed while the default strict client
// must reject the untrusted certificate. This exercises the crypto/tls path
// mirrored from the Spinnaker adapter.
func TestNewClientInsecureTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeBody(w, testApplicationJSON())
	}))
	defer srv.Close()

	// InsecureSkipVerify accepts the self-signed httptest certificate.
	insecure, err := NewClient(Config{Endpoint: srv.URL, token: "t", InsecureSkipVerify: true, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewClient(insecure) error = %v", err)
	}
	if _, err := insecure.GetApplication(newTestContext(t), "my-app"); err != nil {
		t.Fatalf("GetApplication() over InsecureSkipVerify TLS error = %v", err)
	}

	// The default strict client must reject the untrusted self-signed cert.
	strict, err := NewClient(Config{Endpoint: srv.URL, token: "t", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewClient(strict) error = %v", err)
	}
	if _, err := strict.GetApplication(newTestContext(t), "my-app"); err == nil {
		t.Error("GetApplication() over untrusted TLS error = nil, want a certificate verification error")
	}
}

// TestNewClientNoEndpoint verifies that constructing a client without an
// endpoint is rejected, so the integration can never form a request against an
// empty base URL.
func TestNewClientNoEndpoint(t *testing.T) {
	if _, err := NewClient(Config{Endpoint: ""}); err == nil {
		t.Error("NewClient(empty endpoint) error = nil, want error")
	}
}

// TestPatchApplicationAnnotationSuccess verifies the best-effort write-back
// request shape after the C-01/M-03 envelope fix: a PATCH to the plain
// application path (no ?project= query) carrying the application/json content
// type and the bearer header, whose body is the Argo CD ApplicationPatchRequest
// envelope. The envelope's name must be the target Application, patchType must
// be "merge", the configured project must travel INSIDE the body, and the inner
// "patch" field must be a JSON *string* that, once decoded, sets exactly the
// requested metadata annotation.
func TestPatchApplicationAnnotationSuccess(t *testing.T) {
	const (
		wantToken   = "test-token"
		wantProject = "team-a"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("request method = %q, want %q", r.Method, http.MethodPatch)
		}
		if got, want := r.URL.Path, "/api/v1/applications/my-app"; got != want {
			t.Errorf("patch path = %q, want %q", got, want)
		}
		// The project scope must NOT be a query parameter on a PATCH; it travels
		// inside the envelope body (code review C-01/M-03).
		if got := r.URL.Query().Get("project"); got != "" {
			t.Errorf("patch carried ?project=%q query, want none (project belongs in the body)", got)
		}
		if got, want := r.Header.Get("Content-Type"), "application/json"; got != want {
			t.Errorf("Content-Type header = %q, want %q", got, want)
		}
		assertBearerToken(t, r, wantToken)

		// Decode the ApplicationPatchRequest envelope. The inner merge patch is a
		// JSON *string* in the "patch" field, matching the official contract.
		var env struct {
			Name      string `json:"name"`
			Patch     string `json:"patch"`
			PatchType string `json:"patchType"`
			Project   string `json:"project"`
		}
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			t.Errorf("decoding patch envelope: %v", err)
		}
		if got, want := env.Name, "my-app"; got != want {
			t.Errorf("envelope name = %q, want %q", got, want)
		}
		if got, want := env.PatchType, "merge"; got != want {
			t.Errorf("envelope patchType = %q, want %q", got, want)
		}
		if got, want := env.Project, wantProject; got != want {
			t.Errorf("envelope project = %q, want %q", got, want)
		}

		// The "patch" field is a JSON string; decode it into a mirror of the
		// {metadata:{annotations}} merge patch and confirm the annotation.
		var inner struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal([]byte(env.Patch), &inner); err != nil {
			t.Errorf("decoding inner merge patch %q: %v", env.Patch, err)
		}
		if got, want := inner.Metadata.Annotations["k"], "v"; got != want {
			t.Errorf("annotation[\"k\"] = %q, want %q", got, want)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cl, err := NewClient(Config{Endpoint: srv.URL, token: wantToken, Project: wantProject, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if err := cl.PatchApplicationAnnotation(newTestContext(t), "my-app", "k", "v"); err != nil {
		t.Fatalf("PatchApplicationAnnotation() error = %v", err)
	}
}

// TestPatchApplicationAnnotationNon2xx verifies that a non-2xx response is
// surfaced as an error. In production the tracker swallows this error so a
// failed write-back never blocks a termination; here we assert the client
// itself reports it.
func TestPatchApplicationAnnotationNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cl, err := NewClient(Config{Endpoint: srv.URL, token: "t", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if err := cl.PatchApplicationAnnotation(newTestContext(t), "my-app", "k", "v"); err == nil {
		t.Error("PatchApplicationAnnotation() error = nil, want error on HTTP 500")
	}
}

// TestNewClientRejectsInsecureOrMalformedEndpoints proves the C-06 endpoint
// hardening: NewClient must reject any endpoint that would transmit the bearer
// token in cleartext to a remote host, embed credentials in the URL, or use a
// non-http(s) scheme, while still accepting plain http for loopback hosts (used
// by the in-process httptest servers throughout this suite).
func TestNewClientRejectsInsecureOrMalformedEndpoints(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantErr  bool
	}{
		{name: "plain http remote host", endpoint: "http://argocd.example.com", wantErr: true},
		{name: "endpoint embeds userinfo", endpoint: "http://user:pass@127.0.0.1:8080", wantErr: true},
		{name: "non-http scheme", endpoint: "ftp://127.0.0.1", wantErr: true},
		{name: "missing host", endpoint: "http://", wantErr: true},
		{name: "https remote is allowed", endpoint: "https://argocd.example.com", wantErr: false},
		{name: "plain http loopback is allowed", endpoint: "http://127.0.0.1:8080", wantErr: false},
		{name: "plain http localhost is allowed", endpoint: "http://localhost:8080", wantErr: false},
		// Q-14 endpoint hardening: a base URL carrying a query, a forced query,
		// a fragment, or an opaque form would corrupt the string-joined REST
		// path (.../api/v1/applications/{name}) and is rejected up front.
		{name: "endpoint carries query string", endpoint: "https://argocd.example.com?foo=bar", wantErr: true},
		{name: "endpoint carries forced query", endpoint: "https://argocd.example.com/?", wantErr: true},
		{name: "endpoint carries fragment", endpoint: "https://argocd.example.com#frag", wantErr: true},
		{name: "opaque url", endpoint: "https:argocd.example.com", wantErr: true},
		// A normal base path is still accepted so a base-path deployment behind
		// an ingress (e.g. https://host/argocd) keeps working.
		{name: "https base path is allowed", endpoint: "https://argocd.example.com/argocd", wantErr: false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewClient(Config{Endpoint: tc.endpoint, token: "t", Timeout: 5 * time.Second})
			if tc.wantErr && err == nil {
				t.Errorf("NewClient(%q) error = nil, want an endpoint-validation error", tc.endpoint)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("NewClient(%q) error = %v, want nil", tc.endpoint, err)
			}
		})
	}
}

// TestNewClientTransportPreservesProxyDefaults proves the Q-11 transport fix:
// NewClient clones http.DefaultTransport rather than substituting a bare
// zero-value &http.Transport{}. A bare transport would silently drop proxy
// selection (Proxy == nil), the connection dialer, and idle-connection tuning,
// which would break the client behind an enterprise egress proxy. The clone
// must retain those defaults while overriding only TLS and keeping HTTP/2
// disabled.
func TestNewClientTransportPreservesProxyDefaults(t *testing.T) {
	c, err := NewClient(Config{Endpoint: "https://argocd.example.com", token: "t", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport type = %T, want *http.Transport", c.httpClient.Transport)
	}
	def := http.DefaultTransport.(*http.Transport)

	// Proxy selection from the environment must be preserved (a bare transport
	// would leave this nil and ignore HTTP(S)_PROXY / NO_PROXY).
	if tr.Proxy == nil {
		t.Error("transport Proxy = nil, want the inherited http.ProxyFromEnvironment selector")
	}
	// The dialer and idle-connection tuning must match the cloned defaults.
	if tr.DialContext == nil {
		t.Error("transport DialContext = nil, want the inherited default dialer")
	}
	if tr.MaxIdleConns != def.MaxIdleConns {
		t.Errorf("transport MaxIdleConns = %d, want cloned default %d", tr.MaxIdleConns, def.MaxIdleConns)
	}
	if tr.IdleConnTimeout != def.IdleConnTimeout {
		t.Errorf("transport IdleConnTimeout = %v, want cloned default %v", tr.IdleConnTimeout, def.IdleConnTimeout)
	}
	if tr.TLSHandshakeTimeout != def.TLSHandshakeTimeout {
		t.Errorf("transport TLSHandshakeTimeout = %v, want cloned default %v", tr.TLSHandshakeTimeout, def.TLSHandshakeTimeout)
	}
	// Only the TLS configuration is overridden.
	if tr.TLSClientConfig == nil {
		t.Error("transport TLSClientConfig = nil, want the client's configured *tls.Config")
	}
	// HTTP/2 must remain disabled: a non-nil, empty TLSNextProto prevents the
	// automatic HTTP/2 upgrade even though a custom TLSClientConfig is set.
	if tr.ForceAttemptHTTP2 {
		t.Error("transport ForceAttemptHTTP2 = true, want false (HTTP/2 disabled)")
	}
	if tr.TLSNextProto == nil {
		t.Error("transport TLSNextProto = nil, want a non-nil empty map disabling HTTP/2")
	} else if len(tr.TLSNextProto) != 0 {
		t.Errorf("transport TLSNextProto has %d entries, want 0 (HTTP/2 disabled)", len(tr.TLSNextProto))
	}
}

// TestClientRefusesRedirect proves the C-06 redirect guard: a server that
// answers with a 3xx redirect must NOT be followed, because following it could
// forward the Authorization header to another location. The client surfaces a
// request error instead of chasing the redirect.
func TestClientRefusesRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/somewhere-else", http.StatusFound)
	}))
	defer srv.Close()

	cl, err := NewClient(Config{Endpoint: srv.URL, token: "secret", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if _, err := cl.GetApplication(newTestContext(t), "my-app"); err == nil {
		t.Error("GetApplication() following a redirect error = nil, want a refusing-redirect error")
	}
}

// TestGetApplicationRejectsOversizedBody proves the M-04 fail-closed bound: a
// response body larger than maxApplicationResponseBytes must be rejected with an
// error rather than fully buffered, so a runaway or adversarial response cannot
// exhaust memory. The size guard runs before JSON decoding, so the oversized
// payload need not be valid JSON.
func TestGetApplicationRejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// One byte past the cap is enough to trip the fail-closed guard.
		_, _ = w.Write(make([]byte, maxApplicationResponseBytes+1))
	}))
	defer srv.Close()

	cl, err := NewClient(Config{Endpoint: srv.URL, token: "t", Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	app, err := cl.GetApplication(newTestContext(t), "huge")
	if err == nil {
		t.Fatal("GetApplication() error = nil, want an over-limit error")
	}
	if app != nil {
		t.Errorf("GetApplication() app = %v, want nil on over-limit body", app)
	}
	if !contains(err.Error(), "limit") {
		t.Errorf("error = %q, want it to mention the size limit", err.Error())
	}
}

// TestGetApplicationRejectsMalformedBody proves that a 200 response whose body
// is not valid Application JSON is surfaced as a decode error (and a nil
// Application) rather than silently yielding a zero-valued Application that the
// sync/health gate might misread. This fails closed on a corrupt or unexpected
// payload.
func TestGetApplicationRejectsMalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeBody(w, "this is definitely not json {[")
	}))
	defer srv.Close()

	cl, err := NewClient(Config{Endpoint: srv.URL, token: "t", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	app, err := cl.GetApplication(newTestContext(t), "my-app")
	if err == nil {
		t.Fatal("GetApplication() error = nil, want a decode error for a malformed body")
	}
	if app != nil {
		t.Errorf("GetApplication() app = %v, want nil on decode error", app)
	}
}

// TestGetApplicationRespectsTimeout proves the client's configured timeout is
// wired through: against a server that never responds (it blocks until the
// request is cancelled), a short client Timeout must abort the request with an
// error rather than hanging. The handler unblocks on request-context
// cancellation so the test server closes promptly.
func TestGetApplicationRespectsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // block until the client's timeout cancels the request
	}))
	defer srv.Close()

	cl, err := NewClient(Config{Endpoint: srv.URL, token: "t", Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	start := time.Now()
	if _, err := cl.GetApplication(newTestContext(t), "slow"); err == nil {
		t.Error("GetApplication() against a stalled server error = nil, want a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("GetApplication() took %s, want it to abort near the 50ms client timeout", elapsed)
	}
}

// TestClientStringRedactsToken proves the m-02 secret-hardening fix on the
// Client: formatting a *Client with %v, %+v, %s, or %#v must never disclose the
// bearer token, showing only a redaction placeholder. This closes the accidental
// %v/%+v log-leak footgun on the client object itself (Config redaction is
// covered separately in config_test.go).
func TestClientStringRedactsToken(t *testing.T) {
	const secret = "super-secret-jwt-value"
	cl, err := NewClient(Config{Endpoint: "https://argocd.example.com", token: secret, Project: "team-a", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	for _, verb := range []string{"%v", "%+v", "%s", "%#v"} {
		out := fmt.Sprintf(verb, cl)
		if contains(out, secret) {
			t.Errorf("formatting Client with %q disclosed the bearer token: %s", verb, out)
		}
		if !contains(out, "<redacted>") {
			t.Errorf("formatting Client with %q should show a redaction placeholder, got: %s", verb, out)
		}
	}
}

// TestNewClientCustomCATrust proves the crypto/tls CA-bundle path: when
// Config.CACert points at a PEM bundle that includes the server's certificate,
// the strict client (no InsecureSkipVerify) trusts the server and the request
// succeeds. This exercises the bounded readFileLimited CA read plus RootCAs
// wiring in NewClient.
func TestNewClientCustomCATrust(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeBody(w, testApplicationJSON())
	}))
	defer srv.Close()

	// Encode the httptest server's self-signed certificate as a PEM CA bundle.
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if caPEM == nil {
		t.Fatal("failed to PEM-encode the test server certificate")
	}
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := ioutil.WriteFile(caPath, caPEM, 0600); err != nil {
		t.Fatalf("writing temp CA bundle: %v", err)
	}

	cl, err := NewClient(Config{Endpoint: srv.URL, token: "t", CACert: caPath, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewClient(custom CA) error = %v", err)
	}
	if _, err := cl.GetApplication(newTestContext(t), "my-app"); err != nil {
		t.Errorf("GetApplication() with a trusted custom CA error = %v, want nil", err)
	}
}
