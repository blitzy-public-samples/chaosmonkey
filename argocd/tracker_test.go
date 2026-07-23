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
// unreachable endpoint, no carried target, ineligible target, nil client),
// because term.doTerminate treats a tracker error as fatal to a termination that
// has already been recorded. NewTracker is likewise lenient: it must NEVER
// return a non-nil error, degrading instead to a safe no-op tracker. The success
// path additionally asserts the write-back payload: a PATCH carrying the
// chaosmonkey.netflix.com/last-termination annotation whose value JSON-decodes
// to {instance, time, leashed} — the record that surfaces the chaos action on
// the Argo CD timeline (User Example Flow 2).
//
// Carried-target model (QA findings F3/F4/F5): the tracker no longer
// independently re-resolves Application ownership. term.doTerminate carries the
// gate-resolved governing Application name forward on Termination.Target, and the
// tracker writes back to exactly that identity — issuing NO resolution GET, only
// the annotation PATCH. These tests therefore set Termination.Target directly and
// assert the tracker honors it (writes to that Application, skips when it is
// empty or ineligible, and never issues a resolution GET). The correctness of the
// resolution itself (global uniqueness, ambiguity/partial-error/name-mismatch
// denial) is proven where it now lives — the gate — in precheck_test.go and
// mapper_test.go.
//
// Framework: these tests use ONLY the standard-library testing and
// net/http/httptest packages, matching every other test in this module (no test
// file imports testify). testify v1.8.1 appears in go.mod solely as an indirect
// requirement, and its own required modules (e.g. pmezard/go-difflib,
// gopkg.in/yaml.v3) are absent, so importing it directly would make `go mod tidy`
// add those modules and promote testify to a direct require — a go.mod/go.sum
// change the feature's minimal-change contract forbids. Plain stdlib testing is
// therefore the required choice.
//
// Adaptation note: because the tracker writes back to the carried
// Termination.Target rather than re-resolving, the success/error tests configure
// argocd.enabled, an endpoint, a (fake) token, and an explicit
// argocd.applications allow-list (whose membership the tracker re-checks with
// Mapper.IsEligible as belt-and-suspenders), set Termination.Target to the
// authorized Application name, and assert a real PATCH write-back is exercised
// end-to-end through the public NewTracker constructor.

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
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
//
// Target is set to "my-app" — the governing Application name the sync/health gate
// would have resolved and authorized. The tracker writes back to this carried
// identity directly (no resolution GET), so the tests that exercise a real PATCH
// pair this with an allow-list that includes "my-app".
func stdTermination() chaosmonkey.Termination {
	return chaosmonkey.Termination{
		Instance: mock.Instance{App: "foo", Cluster: "my-app", InstanceID: "i-123"},
		Time:     fixedTerminationTime(),
		Leashed:  false,
		Target:   "my-app",
	}
}

// capturedPatch is a mutex-free snapshot of a recorded PATCH request, safe to
// copy and inspect after Track returns. envelopeName and patchType are the outer
// ApplicationPatchRequest fields, captured so the tracker tests can prove the
// write-back targets the RESOLVED Application by name via a "merge" patch
// (code review C-01/C-04).
type capturedPatch struct {
	seen         bool
	method       string
	path         string
	contentType  string
	envelopeName string
	patchType    string
	annotations  map[string]string
}

// patchCapture records the annotation write-back (PATCH) request observed by a
// test server. Only PATCH requests are recorded; a write-back that never happens
// leaves the capture empty (seen == false). (The reworked tracker issues no
// resolution GET — it writes back to the carried Termination.Target — so a GET
// arriving at one of these servers would itself indicate a regression.) It is
// mutex-guarded so an assertion made after Track returns is race-clean even
// though httptest serves each request on its own goroutine.
type patchCapture struct {
	mu           sync.Mutex
	seen         bool
	method       string
	path         string
	contentType  string
	envelopeName string
	patchType    string
	annotations  map[string]string
}

// record stores the observed PATCH request under the lock.
func (c *patchCapture) record(method, path, contentType, envelopeName, patchType string, annotations map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = true
	c.method = method
	c.path = path
	c.contentType = contentType
	c.envelopeName = envelopeName
	c.patchType = patchType
	c.annotations = annotations
}

// snapshot returns a race-clean, lock-free copy of the captured PATCH state.
func (c *patchCapture) snapshot() capturedPatch {
	c.mu.Lock()
	defer c.mu.Unlock()
	return capturedPatch{
		seen:         c.seen,
		method:       c.method,
		path:         c.path,
		contentType:  c.contentType,
		envelopeName: c.envelopeName,
		patchType:    c.patchType,
		annotations:  c.annotations,
	}
}

// newServer returns an httptest.Server that emulates the subset of the Argo CD
// REST API used in these tests. A GET is answered with getBody when getStatus is
// 200, or with a bare getStatus otherwise (retained for completeness; the
// reworked tracker issues no resolution GET). A PATCH (annotation write-back) is
// answered with patchStatus and, when pc is non-nil, its method, path,
// application/json Content-Type, and decoded annotations are recorded into pc.
// Any other method yields 405.
//
// The PATCH body is the Argo CD ApplicationPatchRequest envelope introduced by
// code review C-01/M-03: an outer {name, patch, patchType, project} object whose
// "patch" field is a JSON *string* carrying the inner {metadata:{annotations}}
// merge patch. The handler therefore decodes the envelope first and then unmarshals
// the inner patch string to recover the annotations the test asserts on.
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
			// Decode the ApplicationPatchRequest envelope (name, patchType, and
			// the inner merge patch which is itself a JSON *string*), then unmarshal
			// that inner patch into the annotations map. A decode error at either
			// level simply leaves the fields zero/nil, which the success assertion
			// would then catch.
			var env struct {
				Name      string `json:"name"`
				PatchType string `json:"patchType"`
				Patch     string `json:"patch"`
			}
			_ = json.NewDecoder(r.Body).Decode(&env)
			var body struct {
				Metadata struct {
					Annotations map[string]string `json:"annotations"`
				} `json:"metadata"`
			}
			_ = json.Unmarshal([]byte(env.Patch), &body)
			if pc != nil {
				pc.record(r.Method, r.URL.Path, r.Header.Get("Content-Type"), env.Name, env.PatchType, body.Metadata.Annotations)
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
	if want := "application/json"; got.contentType != want {
		t.Errorf("write-back Content-Type = %q, want %q", got.contentType, want)
	}
	// The outer ApplicationPatchRequest must target the resolved Application by
	// name via a "merge" patch (code review C-01/C-04).
	if want := "my-app"; got.envelopeName != want {
		t.Errorf("write-back envelope name = %q, want %q", got.envelopeName, want)
	}
	if want := "merge"; got.patchType != want {
		t.Errorf("write-back patchType = %q, want %q", got.patchType, want)
	}

	raw, ok := got.annotations[annotationKey]
	if !ok {
		t.Fatalf("PATCH did not carry annotation %q; annotations=%v", annotationKey, got.annotations)
	}

	// The annotation value is itself a compact JSON document; assert it against
	// the terminated instance's id, RFC3339 time, leashed flag, and the truthful
	// pre-execution phase marker (code review M-02).
	var ann struct {
		Instance string `json:"instance"`
		Time     string `json:"time"`
		Leashed  bool   `json:"leashed"`
		Phase    string `json:"phase"`
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
	if want := annotationPhasePreExecution; ann.Phase != want {
		t.Errorf("annotation phase = %q, want %q (write-back records a pre-execution attempt, M-02)", ann.Phase, want)
	}
}

// TestTrack_ServerError_ReturnsNil proves Track swallows every annotation-write
// HTTP-status failure (PATCH 5xx/4xx) and still returns nil, so a write-back
// error can never fail a termination. The tracker issues no resolution GET, so
// only the PATCH status is varied here.
func TestTrack_ServerError_ReturnsNil(t *testing.T) {
	cases := []struct {
		name        string
		patchStatus int
	}{
		{"PATCH 500 internal server error", http.StatusInternalServerError},
		{"PATCH 404 application missing", http.StatusNotFound},
		{"PATCH 403 forbidden (wrong project)", http.StatusForbidden},
		{"PATCH 409 conflict", http.StatusConflict},
		{"PATCH 401 unauthorized", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// getStatus is irrelevant now (the tracker issues no GET); the
			// annotation PATCH is answered with the failing status.
			srv := newServer(http.StatusOK, resolvableAppJSON, tc.patchStatus, nil)
			defer srv.Close()

			tr, err := NewTracker(enabledMonkey(srv.URL, []string{"my-app"}))
			if err != nil {
				t.Fatalf("NewTracker returned error %v, want nil", err)
			}
			if err := tr.Track(stdTermination()); err != nil {
				t.Fatalf("Track returned %v, want nil (a write-back HTTP error must never fail a termination)", err)
			}
		})
	}
}

// TestTrack_Unreachable_ReturnsNil proves that when the Argo CD endpoint is
// unreachable (the server has been closed, so connections are refused) the
// annotation PATCH fails and Track still returns nil.
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

// TestTrack_IneligibleTarget_SkipsWriteBack proves the tracker's defense-in-depth
// eligibility re-check: when the carried Termination.Target is not one of the
// operator-configured eligible Applications, the tracker skips the write-back
// entirely (no PATCH) and still returns nil. Here the allow-list is ["other"] but
// the carried target is "my-app", so Mapper.IsEligible("my-app") is false and the
// write-back is refused before any network call.
func TestTrack_IneligibleTarget_SkipsWriteBack(t *testing.T) {
	pc := &patchCapture{}
	srv := newServer(http.StatusOK, resolvableAppJSON, http.StatusOK, pc)
	defer srv.Close()

	// Allow-list ["other"] does NOT include the carried target "my-app".
	tr, err := NewTracker(enabledMonkey(srv.URL, []string{"other"}))
	if err != nil {
		t.Fatalf("NewTracker returned error %v, want nil", err)
	}
	if err := tr.Track(stdTermination()); err != nil {
		t.Fatalf("Track for an ineligible target returned %v, want nil", err)
	}
	if got := pc.snapshot(); got.seen {
		t.Errorf("expected NO write-back PATCH when the carried target is not eligible, but the server recorded one: %+v", got)
	}
}

// TestTrack_NoCarriedTarget_SkipsWriteBack proves the tracker never guesses a
// target: when Termination.Target is empty (or whitespace-only) — e.g. the gate
// resolved nothing — the tracker skips the write-back entirely (no PATCH) and
// returns nil. This is the tracker side of the F3 guarantee.
func TestTrack_NoCarriedTarget_SkipsWriteBack(t *testing.T) {
	for _, target := range []string{"", "   "} {
		t.Run("target="+target, func(t *testing.T) {
			pc := &patchCapture{}
			srv := newServer(http.StatusOK, resolvableAppJSON, http.StatusOK, pc)
			defer srv.Close()

			tr, err := NewTracker(enabledMonkey(srv.URL, []string{"my-app"}))
			if err != nil {
				t.Fatalf("NewTracker returned error %v, want nil", err)
			}
			trm := stdTermination()
			trm.Target = target
			if err := tr.Track(trm); err != nil {
				t.Fatalf("Track with empty target %q returned %v, want nil", target, err)
			}
			if got := pc.snapshot(); got.seen {
				t.Errorf("expected NO write-back PATCH when no target is carried, but the server recorded one: %+v", got)
			}
		})
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

// -----------------------------------------------------------------------------
// Adversarial write-back tests (QA findings F3/F4/F5; code review C-04, M-01,
// M-02). These prove the tracker writes back to EXACTLY the carried
// Termination.Target the gate authorized — issuing no resolution GET (F4) and
// never redirecting to a different Application even if ownership changed after
// the gate (F5) — bounds the write-back on the pre-kill path (M-01), and records
// a truthful pre-execution attempt (M-02) — all while never surfacing an error.
// (Resolution correctness itself — global uniqueness, ambiguity/partial-error/
// name-mismatch denial — is proven at the gate in precheck_test.go and
// mapper_test.go, which is where re-resolution now exclusively lives.)
//
// Some of these still use a path-aware server that captures PATCH requests and
// can echo a chosen metadata.name (to exercise the client's PATCH-response name
// check); the path-aware fixtures (appResponse, appPathPrefix, oneWorkloadApp)
// are declared in precheck_test.go and reused here (same package).
// -----------------------------------------------------------------------------

// newCapturingMultiAppServer starts a path-aware server that answers
// GET /api/v1/applications/{name} per-name from routes (404 for unknown names),
// records every PATCH into pc, and answers each PATCH 200 with a body echoing a
// metadata.name. By default the echoed name is the requested envelope name (the
// honest, matching case); patchRespName overrides it per requested name so a
// test can force a PATCH response-name mismatch (code review C-04 PATCH
// defense-in-depth). The caller must defer srv.Close().
func newCapturingMultiAppServer(t *testing.T, routes map[string]appResponse, pc *patchCapture, patchRespName map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			name := strings.TrimPrefix(r.URL.Path, appPathPrefix)
			resp, ok := routes[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if resp.status != http.StatusOK {
				w.WriteHeader(resp.status)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(resp.body))
		case http.MethodPatch:
			var env struct {
				Name      string `json:"name"`
				PatchType string `json:"patchType"`
				Patch     string `json:"patch"`
			}
			_ = json.NewDecoder(r.Body).Decode(&env)
			var body struct {
				Metadata struct {
					Annotations map[string]string `json:"annotations"`
				} `json:"metadata"`
			}
			_ = json.Unmarshal([]byte(env.Patch), &body)
			if pc != nil {
				pc.record(r.Method, r.URL.Path, r.Header.Get("Content-Type"), env.Name, env.PatchType, body.Metadata.Annotations)
			}
			echo := env.Name
			if patchRespName != nil {
				if n, ok := patchRespName[env.Name]; ok {
					echo = n
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"metadata":{"name":"` + echo + `"}}`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
}

// newArgoTracker builds a white-box argoTracker wired to a real Client aimed at
// srv, with the given eligibility allow-list and write-back timeout. It bypasses
// NewTracker so a test can set an arbitrarily small timeout to exercise the
// bounded write-back budget quickly (code review M-01).
func newArgoTracker(t *testing.T, srv *httptest.Server, apps []string, timeout time.Duration) argoTracker {
	t.Helper()
	return argoTracker{
		client:  mustClient(t, srv.URL, apps),
		mapper:  NewMapper(apps),
		timeout: timeout,
	}
}

// TestWriteBackBudget proves the M-01 cap: a best-effort write-back is bounded by
// the SMALLER of the configured Argo CD timeout and maxWriteBackTimeout, with a
// non-positive configured value falling back to the cap. This is what keeps the
// pre-kill tracker loop from stalling for the (much larger) general timeout that
// also governs the gate.
func TestWriteBackBudget(t *testing.T) {
	cases := []struct {
		name       string
		configured time.Duration
		want       time.Duration
	}{
		{"large configured is capped", 30 * time.Second, maxWriteBackTimeout},
		{"just over the cap is capped", maxWriteBackTimeout + time.Millisecond, maxWriteBackTimeout},
		{"at the cap is unchanged", maxWriteBackTimeout, maxWriteBackTimeout},
		{"under the cap is unchanged", 2 * time.Second, 2 * time.Second},
		{"zero falls back to the cap", 0, maxWriteBackTimeout},
		{"negative falls back to the cap", -1 * time.Second, maxWriteBackTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := writeBackBudget(tc.configured); got != tc.want {
				t.Errorf("writeBackBudget(%v) = %v, want %v", tc.configured, got, tc.want)
			}
		})
	}
}

// TestTrack_UsesCarriedTarget_NoResolutionGET is the core proof for QA findings
// F3/F4/F5: the tracker writes back to EXACTLY the carried Termination.Target and
// issues NO target-resolution GET. Although three Applications are eligible and
// the terminated instance's identifiers do NOT name appB, the tracker consults
// no GET to derive ownership — it PATCHes appB solely because that is the
// gate-authorized target. A counting server observes zero GETs and exactly one
// PATCH, to appB.
//
// This replaces the previous instance-only re-resolution tracker tests
// (single-owner, cross-app ambiguity, partial-lookup, returned-name-mismatch):
// re-resolution no longer happens in the tracker, so those scenarios are now
// proven at the gate in precheck_test.go (TestAllow_SingleOwnerAmongMany_Allows,
// TestAllow_CrossAppAmbiguity_Denies, TestAllow_PartialLookupError_Denies,
// TestAllow_ReturnedNameMismatch_Denies) and mapper_test.go.
func TestTrack_UsesCarriedTarget_NoResolutionGET(t *testing.T) {
	var mu sync.Mutex
	var getCount, patchCount int
	var patchPath, patchName string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// A resolution GET here would itself be an F4 regression; count it
			// (the assertions below fail if it is non-zero) but still answer so a
			// stray call cannot masquerade as a connection error.
			mu.Lock()
			getCount++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(oneWorkloadApp("appB", SyncStatusSynced, HealthStatusHealthy, "web-b")))
		case http.MethodPatch:
			var env struct {
				Name  string `json:"name"`
				Patch string `json:"patch"`
			}
			_ = json.NewDecoder(r.Body).Decode(&env)
			mu.Lock()
			patchCount++
			patchPath = r.URL.Path
			patchName = env.Name
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"metadata":{"name":"` + env.Name + `"}}`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	tr := newArgoTracker(t, srv, []string{"appA", "appB", "appC"}, 2*time.Second)
	// The gate authorized appB; carry that identity forward. The instance
	// identifiers deliberately do NOT name appB, proving the tracker uses the
	// carried target rather than re-deriving one from the instance.
	trm := chaosmonkey.Termination{
		Instance: mock.Instance{App: "foo", Cluster: "web-b", InstanceID: "i-b"},
		Time:     fixedTerminationTime(),
		Target:   "appB",
	}
	if err := tr.Track(trm); err != nil {
		t.Fatalf("Track returned %v, want nil", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if getCount != 0 {
		t.Errorf("tracker issued %d resolution GET(s), want 0 (F4: no re-resolution)", getCount)
	}
	if patchCount != 1 {
		t.Errorf("tracker issued %d PATCH(es), want exactly 1", patchCount)
	}
	if want := appPathPrefix + "appB"; patchPath != want {
		t.Errorf("write-back path = %q, want %q (must target the carried, gate-authorized Application)", patchPath, want)
	}
	if patchName != "appB" {
		t.Errorf("write-back envelope name = %q, want %q", patchName, "appB")
	}
}

// TestTrack_PatchResponseNameMismatch_ReturnsNil proves the C-04 PATCH
// defense-in-depth still holds under the carried-target model: the PATCH to the
// carried target "appA" is issued, but the PATCH response echoes a DIFFERENT
// metadata.name ("hijacked") — a rename/misroute at write time. The client
// rejects the untrustworthy write-back with an error, which the best-effort
// tracker swallows and returns nil.
func TestTrack_PatchResponseNameMismatch_ReturnsNil(t *testing.T) {
	pc := &patchCapture{}
	// The routes GET fixture is unused by the reworked tracker (it issues no
	// resolution GET); only the PATCH-response name override matters here — it
	// forces the PATCH response for "appA" to echo a different metadata.name.
	srv := newCapturingMultiAppServer(t, nil, pc, map[string]string{"appA": "hijacked"})
	defer srv.Close()

	tr := newArgoTracker(t, srv, []string{"appA"}, 2*time.Second)
	// The gate authorized appA; carry that identity forward.
	trm := chaosmonkey.Termination{
		Instance: mock.Instance{App: "foo", Cluster: "web-a", InstanceID: "i-a"},
		Time:     fixedTerminationTime(),
		Target:   "appA",
	}
	if err := tr.Track(trm); err != nil {
		t.Fatalf("Track returned %v, want nil even when the PATCH response name mismatches", err)
	}
	// The PATCH was attempted (best-effort), but its result was rejected by the
	// client; the tracker still returns nil.
	if got := pc.snapshot(); !got.seen {
		t.Error("expected the PATCH to be attempted before the response-name check rejected it")
	}
}

// TestTrack_SlowServer_BoundedReturnsNil proves M-01 end-to-end: with a small
// write-back budget, a server that stalls far longer does not block the tracker
// for the full stall — the bounded context cancels the request and the tracker
// swallows the resulting error and returns nil well within the stall window.
func TestTrack_SlowServer_BoundedReturnsNil(t *testing.T) {
	stall := 750 * time.Millisecond
	budget := 100 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stall every request (the annotation PATCH) beyond the budget.
		time.Sleep(stall)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resolvableAppJSON))
	}))
	defer srv.Close()

	tr := newArgoTracker(t, srv, []string{"my-app"}, budget)

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- tr.Track(stdTermination()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Track returned %v, want nil (best-effort under a slow server)", err)
		}
		if elapsed := time.Since(start); elapsed >= stall {
			t.Errorf("Track took %v, expected it to be bounded well below the %v server stall", elapsed, stall)
		}
	case <-time.After(stall):
		t.Fatalf("Track did not return within the %v server stall; the write-back budget did not bound it", stall)
	}
}

// TestTrack_SanitizesInstanceIDInSkipLog proves the Q-15 log-injection hardening
// at the tracker's skip-log call site: when a write-back is skipped because the
// termination carries no resolved target, the skip log includes the
// termination's instance id. That id originates outside this package, so an
// embedded newline must be escaped and must never forge an additional log line.
// The tracker still returns nil (best-effort behavior is unchanged).
func TestTrack_SanitizesInstanceIDInSkipLog(t *testing.T) {
	// An enabled tracker (non-nil client) whose termination carries an EMPTY
	// target takes the no-target skip path, which logs the skip with the instance
	// id (the sanitized call site). No network call is made on that path, so the
	// server is never contacted.
	srv := newServer(http.StatusOK, resolvableAppJSON, http.StatusOK, nil)
	defer srv.Close()

	tr, err := NewTracker(enabledMonkey(srv.URL, []string{"my-app"}))
	if err != nil {
		t.Fatalf("NewTracker returned error %v, want nil", err)
	}

	// Capture the standard logger's output for the duration of this call, with
	// no timestamp prefix so the assertions inspect only the message text.
	var buf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()

	trm := chaosmonkey.Termination{
		Instance: mock.Instance{App: "foo", Cluster: "my-app", InstanceID: "i-123\nFAKE forged log line"},
		Time:     fixedTerminationTime(),
		// Target left empty to take the no-target skip path.
	}
	if err := tr.Track(trm); err != nil {
		// Ensure the logger is restored even on a fatal assertion.
		log.SetOutput(prevOut)
		t.Fatalf("Track returned error %v, want nil (best-effort)", err)
	}

	out := buf.String()
	// The embedded newline from the instance id must have been escaped: the raw
	// two-line sequence must NOT appear (log adds only its own single trailing
	// newline), but the escaped \n form must.
	if strings.Contains(out, "i-123\nFAKE forged log line") {
		t.Errorf("skip log contains a raw newline from the instance id (log-injection not prevented):\n%q", out)
	}
	if !strings.Contains(out, `i-123\nFAKE forged log line`) {
		t.Errorf("skip log %q, want the instance id newline escaped as \\n", out)
	}
}
