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

// White-box, table-driven tests for tracker.go. They lock in the best-effort,
// non-blocking write-back contract that Capability C depends on:
//
//   - Track ALWAYS returns nil — on success and on EVERY failure mode (server
//     5xx/4xx, unreachable endpoint, unresolved Application, nil client,
//     nil/typed-nil instance) — because term.doTerminate treats a tracker error
//     as fatal to a termination that has already been recorded;
//   - on success the PATCH carries the chaosmonkey.netflix.com/last-termination
//     annotation whose value JSON-decodes to {instance, time, leashed};
//   - NewTracker is lenient: it never returns a non-nil error and degrades to a
//     no-op tracker on any configuration/client problem;
//   - a nil/typed-nil instance short-circuits before any HTTP call.
//
// The tests use only the standard-library testing and net/http/httptest
// packages with a table-driven style, matching the rest of the repository's
// test suite (testify is not used anywhere in the module). The typed-nil helper
// type nilableInstance is declared in mapper_test.go (same package).

import (
	"encoding/json"
	"io/ioutil"
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
// factory (case "argocd") and the termination loop depend on.
var _ chaosmonkey.Tracker = argoTracker{}

// resolvableAppJSON is the GET /api/v1/applications/myapp response body for an
// Application named "myapp" that manages exactly one live Deployment workload
// also named "myapp". It resolves mock.Instance{Cluster:"myapp"} to "myapp"
// through Mapper.ManagedResourceFor.
const resolvableAppJSON = `{
  "metadata": {"name": "myapp"},
  "status": {
    "sync":   {"status": "Synced"},
    "health": {"status": "Healthy"},
    "resources": [
      {"group": "apps", "kind": "Deployment", "name": "myapp", "health": {"status": "Healthy"}}
    ]
  }
}`

// patchCapture records the most recent PATCH request body seen by a test server.
// It is mutex-guarded so the assertion (after Track returns) is race-clean.
type patchCapture struct {
	mu   sync.Mutex
	got  bool
	body []byte
}

// snapshot returns whether a PATCH was seen and its captured body.
func (c *patchCapture) snapshot() (bool, []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got, c.body
}

// newServer returns an httptest server that answers GET with getStatus (serving
// getBody when 200) and PATCH with patchStatus (recording the body into cap when
// cap is non-nil). It mimics the subset of the Argo CD REST API the tracker uses.
func newServer(getStatus int, getBody string, patchStatus int, cap *patchCapture) *httptest.Server {
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
			body, _ := ioutil.ReadAll(r.Body)
			if cap != nil {
				cap.mu.Lock()
				cap.got = true
				cap.body = body
				cap.mu.Unlock()
			}
			w.WriteHeader(patchStatus)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
}

// mustClient builds a real Argo CD Client aimed at endpoint. It sets the
// unexported token field directly (white-box) so no live auth is required.
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

// TestTrack_SuccessWritesAnnotation verifies the happy path: Track returns nil
// and PATCHes the last-termination annotation whose value decodes to the
// {instance, time, leashed} of the Termination.
func TestTrack_SuccessWritesAnnotation(t *testing.T) {
	cap := &patchCapture{}
	srv := newServer(http.StatusOK, resolvableAppJSON, http.StatusOK, cap)
	defer srv.Close()

	tr := argoTracker{
		client:  mustClient(t, srv.URL, []string{"myapp"}),
		mapper:  NewMapper([]string{"myapp"}),
		timeout: 5 * time.Second,
	}

	when := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	trm := chaosmonkey.Termination{
		Instance: mock.Instance{Cluster: "myapp", InstanceID: "i-123"},
		Time:     when,
		Leashed:  true,
	}

	if err := tr.Track(trm); err != nil {
		t.Fatalf("Track returned non-nil error %v; it must always return nil", err)
	}

	got, body := cap.snapshot()
	if !got {
		t.Fatal("expected a PATCH annotation write, but the server received none")
	}

	var patch struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &patch); err != nil {
		t.Fatalf("could not decode PATCH body %q: %v", string(body), err)
	}

	raw, ok := patch.Metadata.Annotations[annotationKey]
	if !ok {
		t.Fatalf("PATCH did not carry annotation %q; annotations=%v", annotationKey, patch.Metadata.Annotations)
	}

	var ann terminationAnnotation
	if err := json.Unmarshal([]byte(raw), &ann); err != nil {
		t.Fatalf("annotation value %q is not valid JSON: %v", raw, err)
	}
	if ann.Instance != "i-123" {
		t.Errorf("annotation Instance = %q, want %q", ann.Instance, "i-123")
	}
	if want := when.Format(time.RFC3339); ann.Time != want {
		t.Errorf("annotation Time = %q, want %q", ann.Time, want)
	}
	if !ann.Leashed {
		t.Errorf("annotation Leashed = %v, want true", ann.Leashed)
	}
}

// TestTrack_ServerErrorsStillReturnNil verifies Track swallows every HTTP-status
// failure — whether resolution (GET) or the annotation write (PATCH) fails — and
// still returns nil.
func TestTrack_ServerErrorsStillReturnNil(t *testing.T) {
	cases := []struct {
		name        string
		getStatus   int
		patchStatus int
	}{
		{"GET 500 fails resolution", http.StatusInternalServerError, http.StatusOK},
		{"GET 404 missing application", http.StatusNotFound, http.StatusOK},
		{"GET 403 forbidden", http.StatusForbidden, http.StatusOK},
		{"PATCH 500 write fails", http.StatusOK, http.StatusInternalServerError},
		{"PATCH 409 conflict", http.StatusOK, http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newServer(tc.getStatus, resolvableAppJSON, tc.patchStatus, nil)
			defer srv.Close()

			tr := argoTracker{
				client:  mustClient(t, srv.URL, []string{"myapp"}),
				mapper:  NewMapper([]string{"myapp"}),
				timeout: 5 * time.Second,
			}
			trm := chaosmonkey.Termination{
				Instance: mock.Instance{Cluster: "myapp", InstanceID: "i-1"},
				Time:     time.Now(),
			}
			if err := tr.Track(trm); err != nil {
				t.Fatalf("Track returned %v, want nil", err)
			}
		})
	}
}

// TestTrack_UnreachableEndpointReturnsNil verifies that a dead endpoint (server
// closed) causes resolution to fail and Track to return nil.
func TestTrack_UnreachableEndpointReturnsNil(t *testing.T) {
	srv := newServer(http.StatusOK, resolvableAppJSON, http.StatusOK, nil)
	url := srv.URL
	srv.Close() // now unreachable

	tr := argoTracker{
		client:  mustClient(t, url, []string{"myapp"}),
		mapper:  NewMapper([]string{"myapp"}),
		timeout: 1 * time.Second,
	}
	trm := chaosmonkey.Termination{
		Instance: mock.Instance{Cluster: "myapp", InstanceID: "i-1"},
		Time:     time.Now(),
	}
	if err := tr.Track(trm); err != nil {
		t.Fatalf("Track against an unreachable endpoint returned %v, want nil", err)
	}
}

// TestTrack_UnresolvedApplicationReturnsNil verifies Track returns nil when no
// governing Application can be resolved: an empty eligible list, and an eligible
// Application whose resources do not include the target.
func TestTrack_UnresolvedApplicationReturnsNil(t *testing.T) {
	t.Run("empty eligible list", func(t *testing.T) {
		srv := newServer(http.StatusOK, resolvableAppJSON, http.StatusOK, nil)
		defer srv.Close()

		tr := argoTracker{
			client:  mustClient(t, srv.URL, nil),
			mapper:  NewMapper(nil), // no eligible applications
			timeout: 5 * time.Second,
		}
		trm := chaosmonkey.Termination{
			Instance: mock.Instance{Cluster: "myapp", InstanceID: "i-1"},
			Time:     time.Now(),
		}
		if err := tr.Track(trm); err != nil {
			t.Fatalf("Track with empty eligible list returned %v, want nil", err)
		}
	})

	t.Run("eligible application does not manage the instance", func(t *testing.T) {
		const otherApp = `{"metadata":{"name":"myapp"},"status":{"resources":[` +
			`{"group":"apps","kind":"Deployment","name":"someone-else","health":{"status":"Healthy"}}]}}`
		srv := newServer(http.StatusOK, otherApp, http.StatusOK, nil)
		defer srv.Close()

		tr := argoTracker{
			client:  mustClient(t, srv.URL, []string{"myapp"}),
			mapper:  NewMapper([]string{"myapp"}),
			timeout: 5 * time.Second,
		}
		trm := chaosmonkey.Termination{
			Instance: mock.Instance{Cluster: "myapp", InstanceID: "i-1"},
			Time:     time.Now(),
		}
		if err := tr.Track(trm); err != nil {
			t.Fatalf("Track for a non-managing Application returned %v, want nil", err)
		}
	})
}

// TestTrack_NilClientReturnsNil verifies that a tracker with no client (the
// disabled/degraded no-op) returns nil without touching the network.
func TestTrack_NilClientReturnsNil(t *testing.T) {
	var tr argoTracker // zero value: client is nil
	trm := chaosmonkey.Termination{
		Instance: mock.Instance{Cluster: "myapp", InstanceID: "i-1"},
		Time:     time.Now(),
	}
	if err := tr.Track(trm); err != nil {
		t.Fatalf("Track with a nil client returned %v, want nil", err)
	}
}

// TestTrack_NilInstanceReturnsNil verifies both an untyped-nil and a typed-nil
// instance short-circuit before any HTTP call and return nil.
func TestTrack_NilInstanceReturnsNil(t *testing.T) {
	var mu sync.Mutex
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hit = true
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := argoTracker{
		client:  mustClient(t, srv.URL, []string{"myapp"}),
		mapper:  NewMapper([]string{"myapp"}),
		timeout: 5 * time.Second,
	}

	cases := []struct {
		name     string
		instance chaosmonkey.Instance
	}{
		{"untyped-nil instance", nil},
		// nilableInstance is declared in mapper_test.go (same package); a typed
		// nil pointer satisfies chaosmonkey.Instance but would panic if a method
		// were invoked, proving the isNilInstance guard protects the scheduler.
		{"typed-nil instance", chaosmonkey.Instance((*nilableInstance)(nil))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trm := chaosmonkey.Termination{Instance: tc.instance, Time: time.Now()}
			if err := tr.Track(trm); err != nil {
				t.Fatalf("Track with a %s returned %v, want nil", tc.name, err)
			}
		})
	}

	mu.Lock()
	defer mu.Unlock()
	if hit {
		t.Error("Track made an HTTP call despite a nil instance; the nil guard must short-circuit")
	}
}

// TestNewTracker_IsLenient verifies NewTracker never returns a non-nil error and
// degrades to a no-op tracker on configuration problems, while still building a
// working tracker from a complete, enabled configuration.
func TestNewTracker_IsLenient(t *testing.T) {
	t.Run("disabled config yields a no-op tracker and nil error", func(t *testing.T) {
		m := config.Defaults() // argocd.enabled defaults to false
		tr, err := NewTracker(m)
		if err != nil {
			t.Fatalf("NewTracker returned error %v, want nil", err)
		}
		if tr == nil {
			t.Fatal("NewTracker returned a nil tracker")
		}
		trm := chaosmonkey.Termination{
			Instance: mock.Instance{Cluster: "x", InstanceID: "i"},
			Time:     time.Now(),
		}
		if err := tr.Track(trm); err != nil {
			t.Fatalf("disabled tracker Track returned %v, want nil", err)
		}
	})

	t.Run("enabled but missing token degrades and returns nil error", func(t *testing.T) {
		m := config.Defaults()
		m.Set(param.ArgoCDEnabled, true)
		m.Set(param.ArgoCDEndpoint, "https://argocd.example.com")
		m.Set(param.ArgoCDApplications, []string{"myapp"})
		// No token / token_file: configFromMonkey errors, and NewTracker must
		// degrade to a no-op rather than surface the error to the factory.
		tr, err := NewTracker(m)
		if err != nil {
			t.Fatalf("NewTracker returned error %v, want nil (it must be lenient)", err)
		}
		trm := chaosmonkey.Termination{
			Instance: mock.Instance{Cluster: "myapp", InstanceID: "i"},
			Time:     time.Now(),
		}
		if err := tr.Track(trm); err != nil {
			t.Fatalf("degraded tracker Track returned %v, want nil", err)
		}
	})

	t.Run("complete enabled config builds a working tracker", func(t *testing.T) {
		cap := &patchCapture{}
		srv := newServer(http.StatusOK, resolvableAppJSON, http.StatusOK, cap)
		defer srv.Close()

		m := config.Defaults()
		m.Set(param.ArgoCDEnabled, true)
		m.Set(param.ArgoCDEndpoint, srv.URL)
		m.Set(param.ArgoCDToken, "test-token")
		m.Set(param.ArgoCDApplications, []string{"myapp"})

		tr, err := NewTracker(m)
		if err != nil {
			t.Fatalf("NewTracker returned error %v, want nil", err)
		}
		trm := chaosmonkey.Termination{
			Instance: mock.Instance{Cluster: "myapp", InstanceID: "i-9"},
			Time:     time.Now(),
		}
		if err := tr.Track(trm); err != nil {
			t.Fatalf("Track returned %v, want nil", err)
		}
		if got, _ := cap.snapshot(); !got {
			t.Error("expected the NewTracker-built tracker to write an annotation on success")
		}
	})
}
