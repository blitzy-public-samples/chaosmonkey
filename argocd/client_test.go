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
//   - PatchApplicationAnnotation: the PATCH method, the JSON merge-patch content
//     type, the bearer header, the {metadata:{annotations}} body shape, and the
//     non-2xx error path (which the tracker swallows in production).
//
// The tests use only the standard-library testing package with a table-driven
// style, matching the rest of the repository's test suite (testify is not used
// anywhere in the module, and importing it would require a go.mod change).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		if got, want := r.Header.Get("Authorization"), "Bearer "+wantToken; got != want {
			t.Errorf("Authorization header = %q, want %q", got, want)
		}
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
		if got, want := r.Header.Get("Authorization"), "Bearer "+wantToken; got != want {
			t.Errorf("Authorization header = %q, want %q", got, want)
		}
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
// request shape: a PATCH carrying the JSON merge-patch content type and the
// bearer header, with a body that sets exactly the requested metadata
// annotation.
func TestPatchApplicationAnnotationSuccess(t *testing.T) {
	const wantToken = "test-token"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("request method = %q, want %q", r.Method, http.MethodPatch)
		}
		if got, want := r.Header.Get("Content-Type"), "application/merge-patch+json"; got != want {
			t.Errorf("Content-Type header = %q, want %q", got, want)
		}
		if got, want := r.Header.Get("Authorization"), "Bearer "+wantToken; got != want {
			t.Errorf("Authorization header = %q, want %q", got, want)
		}

		// Decode the merge patch into a local mirror of the {metadata:{annotations}}
		// body shape and confirm the single requested annotation is present.
		var body struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding patch body: %v", err)
		}
		if got, want := body.Metadata.Annotations["k"], "v"; got != want {
			t.Errorf("annotation[\"k\"] = %q, want %q", got, want)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cl, err := NewClient(Config{Endpoint: srv.URL, token: wantToken, Timeout: 5 * time.Second})
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
