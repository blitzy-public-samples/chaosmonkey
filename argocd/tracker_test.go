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

// White-box unit tests for tracker.go. They are the executable proof of the
// best-effort, NON-BLOCKING write-back contract (AAP 0.5.3): argoTracker.Track
// MUST return nil on success AND on every failure mode (server 5xx/4xx,
// unreachable endpoint, unresolved Application, nil client), because
// term.doTerminate treats a tracker error as fatal to a termination that has
// already been recorded. NewTracker is likewise lenient: it must NEVER return a
// non-nil error, degrading instead to a safe no-op tracker. The success path
// additionally asserts the write-back payload: a PATCH carrying the
// chaosmonkey.netflix.com/last-termination annotation whose value JSON-decodes
// to {instance, time, leashed} — the record that surfaces the chaos action on
// the Argo CD timeline (User Example Flow 2).
//
// Framework: these tests use ONLY the standard-library testing and
// net/http/httptest packages, matching every other test in this module. testify
// is not imported anywhere in the repository; it appears in go.mod solely as an
// // indirect requirement, so importing it directly here would promote it to a
// direct dependency under `go mod tidy` and change go.mod — which the feature's
// minimal-change contract forbids. Plain testing is therefore the required
// choice (the plan's explicit fallback when testify would force a go.mod/go.sum
// change).
//
// Adaptation note: the delivered target-resolution engine (mapper.go) is
// fail-closed and binds ownership to an Application's authoritative
// status.resources[] inventory — it never infers ownership from a bare
// Application-name coincidence, and an empty eligibility allow-list resolves
// NOTHING. The success/error tests therefore configure argocd.enabled, an
// endpoint, a (fake) token, and an explicit argocd.applications allow-list, and
// serve an Application whose resources map to the target, so a real PATCH
// write-back is exercised end-to-end through the public NewTracker constructor.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Netflix/chaosmonkey/v2"
	"github.com/Netflix/chaosmonkey/v2/config"
	"github.com/Netflix/chaosmonkey/v2/config/param"
	"github.com/Netflix/chaosmonkey/v2/mock"
)

// argoTracker must satisfy the chaosmonkey.Tracker contract that the tracker
// factory (case "argocd") and term.doTerminate's tracker loop depend on.
var _ chaosmonkey.Tracker = argoTracker{}

// resolvableAppJSON is the GET /api/v1/applications/my-app response for an
// Application named "my-app" that manages exactly one live Deployment workload
// also named "my-app". Through Mapper.ManagedResourceFor it resolves a target
// whose cluster (or app) identifier is "my-app", so a write-back PATCH follows.
const resolvableAppJSON = `{
  "metadata": {"name": "my-app"},
  "status": {
    "sync":   {"status": "Synced"},
    "health": {"status": "Healthy"},
    "resources": [
      {"group": "apps", "kind": "Deployment", "name": "my-app", "health": {"status": "Healthy"}}
    ]
  }
}`

// otherAppJSON is an eligible-but-non-matching Application: it is named "other"
// and manages only a workload named "other", so it can never own a target whose
// identifiers are {my-app, foo}. It is used to prove resolution denial without
// any name-coincidence shortcut.
const otherAppJSON = `{
  "metadata": {"name": "other"},
  "status": {
    "sync":   {"status": "Synced"},
    "health": {"status": "Healthy"},
    "resources": [
      {"group": "apps", "kind": "Deployment", "name": "other", "health": {"status": "Healthy"}}
    ]
  }
}`

// fixedTerminationTimeRFC3339 is the RFC3339 rendering of the deterministic
// termination timestamp used by the payload assertion.
const fixedTerminationTimeRFC3339 = "2023-01-02T03:04:05Z"

// fixedTerminationTime returns the deterministic termination timestamp; its
// RFC3339 form is exactly fixedTerminationTimeRFC3339.
func fixedTerminationTime() time.Time {
	return time.Date(2023, time.January, 2, 3, 4, 5, 0, time.UTC)
}

// stdTermination is the canonical termination used across these tests: instance
// i-123 in cluster "my-app" (app "foo"), terminated at the fixed time, not
// leashed. The chaosmonkey.Termination type carries no termination identifier,
// so the instance id is the subject recorded in the annotation.
func stdTermination() chaosmonkey.Termination {
	return chaosmonkey.Termination{
		Instance: mock.Instance{App: "foo", Cluster: "my-app", InstanceID: "i-123"},
		Time:     fixedTerminationTime(),
		Leashed:  false,
	}
}

// capturedPatch is a mutex-free snapshot of a recorded PATCH request, safe to
// copy and inspect after Track returns.
type capturedPatch struct {
	seen        bool
	method      string
	path        string
	contentType string
	annotations map[string]string
}

// patchCapture records the annotation write-back (PATCH) request observed by a
// test server. Only PATCH requests are recorded; the GET issued during target
// resolution is answered but not captured, so a write-back that never happens
// leaves the capture empty (seen == false). It is mutex-guarded so an assertion
// made after Track returns is race-clean even though httptest serves each
// request on its own goroutine.
type patchCapture struct {
	mu          sync.Mutex
	seen        bool
	method      string
	path        string
	contentType string
	annotations map[string]string
}

// record stores the observed PATCH request under the lock.
func (c *patchCapture) record(method, path, contentType string, annotations map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = true
	c.method = method
	c.path = path
	c.contentType = contentType
	c.annotations = annotations
}

// snapshot returns a race-clean, lock-free copy of the captured PATCH state.
func (c *patchCapture) snapshot() capturedPatch {
	c.mu.Lock()
	defer c.mu.Unlock()
	return capturedPatch{
		seen:        c.seen,
		method:      c.method,
		path:        c.path,
		contentType: c.contentType,
		annotations: c.annotations,
	}
}

// newServer returns an httptest.Server that emulates the subset of the Argo CD
// REST API the tracker uses. A GET (target resolution) is answered with getBody
// when getStatus is 200, or with a bare getStatus otherwise. A PATCH (annotation
// write-back) is answered with patchStatus and, when pc is non-nil, its method,
// path, JSON merge-patch Content-Type, and decoded metadata.annotations are
// recorded into pc. Any other method yields 405.
func newServer(getStatus int, getBody string, patchStatus int, pc *patchCapture) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if getStatus != http.StatusOK {
				w.WriteHeader(getStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(getBody))
		case http.MethodPatch:
			// Decode the JSON merge patch straight from the request body. A
			// decode error simply leaves the annotations map nil, which the
			// success assertion would then catch.
			var body struct {
				Metadata struct {
					Annotations map[string]string `json:"annotations"`
				} `json:"metadata"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if pc != nil {
				pc.record(r.Method, r.URL.Path, r.Header.Get("Content-Type"), body.Metadata.Annotations)
			}
			w.WriteHeader(patchStatus)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
}

// enabledMonkey returns a Chaos Monkey configuration with the Argo CD write-back
// fully enabled, pointed at endpoint, with the supplied eligible-Application
// allow-list and an obvious fake bearer token (never a real credential).
// argocd.enabled is required because configFromMonkey is fail-closed: with the
// feature disabled it returns an inert snapshot and the tracker degrades to a
// nil-client no-op that never performs a write-back.
func enabledMonkey(endpoint string, applications []string) *config.Monkey {
	m := config.Defaults()
	m.Set(param.ArgoCDEnabled, true)
	m.Set(param.ArgoCDEndpoint, endpoint)
	m.Set(param.ArgoCDToken, "test-token")
	m.Set(param.ArgoCDApplications, applications)
	return m
}

// mustClient builds a real Argo CD Client aimed at endpoint. It sets the
// unexported token field directly (white-box) so no live auth is required.
// This shared helper is also relied upon by precheck_test.go.
func mustClient(t *testing.T, endpoint string, apps []string) *Client {
	t.Helper()
	c := Config{
		Enabled:      true,
		Endpoint:     endpoint,
		token:        "test-token",
		Applications: apps,
		Timeout:      5 * time.Second,
	}
	client, err := NewClient(c)
	if err != nil {
		t.Fatalf("NewClient(%q) failed: %v", endpoint, err)
	}
	return client
}

// TestTrack_Success_WritesAnnotation proves the happy path: Track returns nil
// and issues a PATCH that records the chaosmonkey.netflix.com/last-termination
// annotation whose value decodes to the termination's {instance, time, leashed}.
func TestTrack_Success_WritesAnnotation(t *testing.T) {
	pc := &patchCapture{}
	srv := newServer(http.StatusOK, resolvableAppJSON, http.StatusOK, pc)
	defer srv.Close()

	tr, err := NewTracker(enabledMonkey(srv.URL, []string{"my-app"}))
	if err != nil {
		t.Fatalf("NewTracker returned error %v, want nil", err)
	}

	// The single acceptable Track return is nil, on success as on failure.
	if err := tr.Track(stdTermination()); err != nil {
		t.Fatalf("Track returned %v, want nil (write-back must never fail a termination)", err)
	}

	got := pc.snapshot()
	if !got.seen {
		t.Fatal("expected a PATCH annotation write-back, but the server received none")
	}
	if got.method != http.MethodPatch {
		t.Errorf("write-back method = %q, want %q", got.method, http.MethodPatch)
	}
	if want := "/api/v1/applications/my-app"; got.path != want {
		t.Errorf("write-back path = %q, want %q", got.path, want)
	}
	if want := "application/merge-patch+json"; got.contentType != want {
		t.Errorf("write-back Content-Type = %q, want %q", got.contentType, want)
	}

	raw, ok := got.annotations[annotationKey]
	if !ok {
		t.Fatalf("PATCH did not carry annotation %q; annotations=%v", annotationKey, got.annotations)
	}

	// The annotation value is itself a compact JSON document; assert it against
	// the terminated instance's id, RFC3339 time, and leashed flag.
	var ann struct {
		Instance string `json:"instance"`
		Time     string `json:"time"`
		Leashed  bool   `json:"leashed"`
	}
	if err := json.Unmarshal([]byte(raw), &ann); err != nil {
		t.Fatalf("annotation value %q is not valid JSON: %v", raw, err)
	}
	if ann.Instance != "i-123" {
		t.Errorf("annotation instance = %q, want %q", ann.Instance, "i-123")
	}
	if ann.Time != fixedTerminationTimeRFC3339 {
		t.Errorf("annotation time = %q, want %q", ann.Time, fixedTerminationTimeRFC3339)
	}
	if ann.Leashed {
		t.Errorf("annotation leashed = %v, want false", ann.Leashed)
	}
}

// TestTrack_ServerError_ReturnsNil proves Track swallows every HTTP-status
// failure — whether target resolution (GET) or the annotation write (PATCH)
// fails — and still returns nil, so a write-back error can never fail a
// termination.
func TestTrack_ServerError_ReturnsNil(t *testing.T) {
	cases := []struct {
		name        string
		getStatus   int
		patchStatus int
	}{
		{"GET 500 fails resolution", http.StatusInternalServerError, http.StatusOK},
		{"GET 404 missing application", http.StatusNotFound, http.StatusOK},
		{"GET 403 forbidden", http.StatusForbidden, http.StatusOK},
		{"PATCH 500 write-back fails", http.StatusOK, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newServer(tc.getStatus, resolvableAppJSON, tc.patchStatus, nil)
			defer srv.Close()

			tr, err := NewTracker(enabledMonkey(srv.URL, []string{"my-app"}))
			if err != nil {
				t.Fatalf("NewTracker returned error %v, want nil", err)
			}
			if err := tr.Track(stdTermination()); err != nil {
				t.Fatalf("Track returned %v, want nil", err)
			}
		})
	}
}

// TestTrack_Unreachable_ReturnsNil proves that when the Argo CD endpoint is
// unreachable (the server has been closed, so connections are refused) target
// resolution fails and Track still returns nil.
func TestTrack_Unreachable_ReturnsNil(t *testing.T) {
	srv := newServer(http.StatusOK, resolvableAppJSON, http.StatusOK, nil)
	deadURL := srv.URL
	srv.Close() // connections to deadURL are now refused

	tr, err := NewTracker(enabledMonkey(deadURL, []string{"my-app"}))
	if err != nil {
		t.Fatalf("NewTracker returned error %v, want nil", err)
	}
	if err := tr.Track(stdTermination()); err != nil {
		t.Fatalf("Track against an unreachable endpoint returned %v, want nil", err)
	}
}

// TestTrack_UnresolvedApplication_ReturnsNil proves that when no governing
// Application can be resolved for the target — here the only eligible
// Application ("other") does not manage the terminated instance — Track returns
// nil and performs NO write-back PATCH (the capture stays empty). Resolution is
// bound to the Application's authoritative status.resources[] inventory, so a
// non-matching eligible Application is correctly rejected before any write.
func TestTrack_UnresolvedApplication_ReturnsNil(t *testing.T) {
	pc := &patchCapture{}
	srv := newServer(http.StatusOK, otherAppJSON, http.StatusOK, pc)
	defer srv.Close()

	// The eligible allow-list is ["other"], but the termination targets cluster
	// "my-app"/app "foo", which "other" does not manage.
	tr, err := NewTracker(enabledMonkey(srv.URL, []string{"other"}))
	if err != nil {
		t.Fatalf("NewTracker returned error %v, want nil", err)
	}
	if err := tr.Track(stdTermination()); err != nil {
		t.Fatalf("Track for an unresolved Application returned %v, want nil", err)
	}
	if got := pc.snapshot(); got.seen {
		t.Errorf("expected NO write-back PATCH when resolution fails, but the server recorded one: %+v", got)
	}
}

// TestTrack_NilClient_ReturnsNil proves that a tracker built from a disabled /
// unconfigured configuration has a nil client and treats Track as an immediate
// no-op that returns nil without touching the network.
func TestTrack_NilClient_ReturnsNil(t *testing.T) {
	// config.Defaults() leaves argocd.enabled false and sets no endpoint, so
	// NewTracker builds an inert tracker whose client is nil.
	tr, err := NewTracker(config.Defaults())
	if err != nil {
		t.Fatalf("NewTracker returned error %v, want nil", err)
	}
	if err := tr.Track(stdTermination()); err != nil {
		t.Fatalf("Track on a nil-client (disabled) tracker returned %v, want nil", err)
	}
}

// TestNewTracker_NeverErrors proves NewTracker is lenient: it returns a nil
// error for a disabled configuration and for every configuration/client
// construction problem, degrading to a safe no-op tracker instead of surfacing
// an error to the tracker factory (which would otherwise abort the entire
// terminate run). In every case the returned tracker's Track is also a safe
// no-op that returns nil.
func TestNewTracker_NeverErrors(t *testing.T) {
	t.Run("disabled default configuration", func(t *testing.T) {
		tr, err := NewTracker(config.Defaults())
		if err != nil {
			t.Fatalf("NewTracker returned error %v, want nil", err)
		}
		if tr == nil {
			t.Fatal("NewTracker returned a nil tracker")
		}
		if err := tr.Track(stdTermination()); err != nil {
			t.Fatalf("disabled tracker Track returned %v, want nil", err)
		}
	})

	t.Run("enabled but client build fails (bad ca_cert) degrades", func(t *testing.T) {
		m := config.Defaults()
		m.Set(param.ArgoCDEnabled, true)
		m.Set(param.ArgoCDEndpoint, "https://argocd.example")
		m.Set(param.ArgoCDToken, "test-token")
		// A non-existent CA bundle makes NewClient fail; NewTracker must log and
		// degrade to a no-op rather than return the error.
		m.Set(param.ArgoCDCACert, "/nonexistent/ca.pem")

		tr, err := NewTracker(m)
		if err != nil {
			t.Fatalf("NewTracker returned error %v, want nil (it must be lenient)", err)
		}
		if err := tr.Track(stdTermination()); err != nil {
			t.Fatalf("degraded tracker Track returned %v, want nil", err)
		}
	})

	t.Run("enabled but missing token degrades", func(t *testing.T) {
		m := config.Defaults()
		m.Set(param.ArgoCDEnabled, true)
		m.Set(param.ArgoCDEndpoint, "https://argocd.example")
		m.Set(param.ArgoCDApplications, []string{"my-app"})
		// No token / token_file: configFromMonkey errors, and NewTracker must
		// degrade to a no-op rather than surface the error to the factory.
		tr, err := NewTracker(m)
		if err != nil {
			t.Fatalf("NewTracker returned error %v, want nil (it must be lenient)", err)
		}
		if err := tr.Track(stdTermination()); err != nil {
			t.Fatalf("degraded tracker Track returned %v, want nil", err)
		}
	})
}
