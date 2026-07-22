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

// White-box, table-driven tests for precheck.go. This is the safety-critical
// validation file: it is the executable proof of the AAP's central contract
// that the Argo CD sync/health gate never permits a termination under any
// uncertainty (mirroring the outage-check's err-on-the-safe-side behavior).
//
// The suite exercises, against an in-process mock Argo CD API built with
// net/http/httptest:
//
//   - ALLOW only when the governing Application is Synced AND Healthy;
//   - DENY (graceful skip, nil error) for every unhealthy/out-of-sync state;
//   - fail-closed DENY on an unreachable API, HTTP 404 / 403 / 5xx, an
//     Application that manages no live workloads, a target that cannot be
//     resolved to any eligible Application, and an empty eligibility allow-list;
//   - allow-all inertness when the integration is disabled, plus the
//     factory -> client -> gate wiring end to end.
//
// Every deny/fail-closed assertion requires allowed==false AND err==nil, so an
// uncertain gate is a silent skip and never a crash or a propagated error.
//
// The tests use only the standard-library testing and net/http/httptest
// packages with a table-driven style, matching the rest of the repository's
// test suite (testify is not used anywhere in the module, and importing it
// would promote an indirect dependency to a direct one, forcing a go.mod
// change). Several helpers declared in the sibling test files of this same
// package are reused: newServer and mustClient (tracker_test.go) and contains
// (client_test.go).

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Netflix/chaosmonkey/v2/config"
	"github.com/Netflix/chaosmonkey/v2/config/param"
	"github.com/Netflix/chaosmonkey/v2/grp"
	"github.com/Netflix/chaosmonkey/v2/mock"
)

// precheckApp is the single eligible Argo CD Application name shared by the test
// fixtures. Keeping the served Application's metadata.name, the mapper
// allow-list entry, and the standard target's identifiers all equal to this one
// value makes a healthy fixture resolve deterministically to its governing
// Application.
const precheckApp = "myapp"

// appJSON renders a GET /api/v1/applications/{name} response body for an
// Application named precheckApp with the supplied aggregate sync and health
// status and exactly resourceCount managed resources.
//
// Every emitted resource is a live "apps/Deployment" workload (per-resource
// health is always Healthy), so a fixture with resourceCount >= 1 is always
// resolvable by the mapper. The FIRST resource is named precheckApp (matching
// the standard target); any additional resources get a distinct, non-matching
// name so that exactly one resource ever correlates to the target and ownership
// is never ambiguous. resourceCount == 0 yields an Application that manages no
// live workloads, exercising the fail-closed "could not resolve" branch.
//
// The aggregate sync/health arguments are what the gate actually inspects; they
// are intentionally decoupled from the (always live) per-resource health so the
// tests isolate the Application-level allow/deny decision from target
// resolution.
func appJSON(sync, health string, resourceCount int) string {
	resources := ""
	for i := 0; i < resourceCount; i++ {
		name := precheckApp
		if i > 0 {
			// A distinct, non-matching name for any extra resource keeps
			// resolution unambiguous (exactly one resource matches the target).
			resources += ","
			name = precheckApp + "-extra"
		}
		resources += `{"group":"apps","version":"v1","kind":"Deployment",` +
			`"namespace":"prod","name":"` + name + `","status":"Synced",` +
			`"health":{"status":"Healthy"}}`
	}
	return `{"metadata":{"name":"` + precheckApp + `","namespace":"argocd"},` +
		`"status":{"sync":{"status":"` + sync + `"},` +
		`"health":{"status":"` + health + `"},` +
		`"resources":[` + resources + `]}}`
}

// appServer starts an httptest server that answers GET
// /api/v1/applications/{name} with the given HTTP status. When status is 200 it
// serves appJSON(sync, health, resourceCount); for any other status it returns
// that status with an empty body, exercising the client's 404 / 403 / 5xx error
// mapping. It reuses newServer (declared in tracker_test.go); the PATCH path is
// irrelevant here because the gate only issues GETs. The caller must
// defer srv.Close().
func appServer(t *testing.T, status int, sync, health string, resourceCount int) *httptest.Server {
	t.Helper()
	return newServer(status, appJSON(sync, health, resourceCount), status, nil)
}

// newArgoPrecheck builds an *argoPrecheck wired to srv with a mapper whose
// allow-list contains exactly precheckApp, so a healthy fixture served by
// appServer resolves to its governing Application. It reuses mustClient
// (declared in tracker_test.go), which sets the unexported bearer token
// directly for white-box testing so no live authentication is required.
func newArgoPrecheck(t *testing.T, srv *httptest.Server) *argoPrecheck {
	t.Helper()
	return &argoPrecheck{
		client:  mustClient(t, srv.URL, nil),
		mapper:  NewMapper([]string{precheckApp}),
		timeout: 2 * time.Second,
	}
}

// standardTarget returns the canonical termination target used across the gate
// tests. Its cluster and app names both equal precheckApp, so it correlates to
// the live Deployment workload named precheckApp in the served Application.
func standardTarget() mock.Instance {
	return mock.Instance{App: precheckApp, Cluster: precheckApp}
}

// assertGracefulSkip asserts that an Allow result is a fail-closed skip: denied
// (allowed==false), with a nil error (the gate must never crash the scheduler or
// propagate an error) and a non-empty, descriptive reason.
func assertGracefulSkip(t *testing.T, allowed bool, reason string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Allow returned error %v, want nil (a fail-closed decision must be a graceful skip, never an error)", err)
	}
	if allowed {
		t.Error("Allow allowed = true, want false (the gate must fail closed under uncertainty)")
	}
	if reason == "" {
		t.Error("Allow reason = empty, want a descriptive skip reason")
	}
}

// -----------------------------------------------------------------------------
// Allow path — the ONLY case that permits a termination is Synced AND Healthy.
// -----------------------------------------------------------------------------

// TestAllow_SyncedHealthy_Allows verifies the single allow path: a governing
// Application that is both Synced and Healthy permits the termination, with no
// error and an empty reason.
func TestAllow_SyncedHealthy_Allows(t *testing.T) {
	srv := appServer(t, http.StatusOK, SyncStatusSynced, HealthStatusHealthy, 1)
	defer srv.Close()

	p := newArgoPrecheck(t, srv)
	allowed, reason, err := p.Allow(nil, standardTarget())
	if err != nil {
		t.Fatalf("Allow returned error %v, want nil", err)
	}
	if !allowed {
		t.Errorf("Allow allowed = false, want true for Synced+Healthy; reason=%q", reason)
	}
	if reason != "" {
		t.Errorf("Allow reason = %q, want empty on allow", reason)
	}
}

// TestAllow_ResolvesViaGroup_Allows verifies that when the target instance
// carries no identifiers of its own, resolution still succeeds via the
// InstanceGroup's app name (which equals the managed workload name), and a
// Synced+Healthy Application is allowed.
func TestAllow_ResolvesViaGroup_Allows(t *testing.T) {
	srv := appServer(t, http.StatusOK, SyncStatusSynced, HealthStatusHealthy, 1)
	defer srv.Close()

	p := newArgoPrecheck(t, srv)
	group := grp.New(precheckApp, "test", "us-east-1", "", "")
	allowed, reason, err := p.Allow(group, mock.Instance{})
	if err != nil {
		t.Fatalf("Allow returned error %v, want nil", err)
	}
	if !allowed {
		t.Errorf("Allow allowed = false, want true (group-resolved Synced+Healthy); reason=%q", reason)
	}
}

// -----------------------------------------------------------------------------
// Deny paths — every unhealthy/out-of-sync state is a graceful skip.
// -----------------------------------------------------------------------------

// TestAllow_DeniesUnhealthyOrOutOfSync verifies that a reachable, resolvable
// Application is denied whenever it is not BOTH Synced and Healthy. Each case
// is a graceful skip (allowed==false, err==nil) whose reason names the offending
// sync and health status so the skip is visible in the scheduler logs.
func TestAllow_DeniesUnhealthyOrOutOfSync(t *testing.T) {
	cases := []struct {
		name   string
		sync   string
		health string
	}{
		{"synced but progressing", SyncStatusSynced, HealthStatusProgressing},
		{"synced but degraded", SyncStatusSynced, HealthStatusDegraded},
		{"synced but suspended", SyncStatusSynced, HealthStatusSuspended},
		{"synced but missing", SyncStatusSynced, HealthStatusMissing},
		{"synced but unknown", SyncStatusSynced, HealthStatusUnknown},
		{"out of sync but healthy", SyncStatusOutOfSync, HealthStatusHealthy},
		{"out of sync and progressing", SyncStatusOutOfSync, HealthStatusProgressing},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv := appServer(t, http.StatusOK, tc.sync, tc.health, 1)
			defer srv.Close()

			p := newArgoPrecheck(t, srv)
			allowed, reason, err := p.Allow(nil, standardTarget())
			if err != nil {
				t.Fatalf("Allow returned error %v, want nil (a deny must be graceful)", err)
			}
			if allowed {
				t.Errorf("Allow allowed = true, want false for sync=%q health=%q", tc.sync, tc.health)
			}
			if reason == "" {
				t.Fatal("Allow reason = empty, want a descriptive deny reason")
			}
			// The reason names both the offending sync and health status.
			if !contains(reason, tc.sync) {
				t.Errorf("reason %q does not mention sync status %q", reason, tc.sync)
			}
			if !contains(reason, tc.health) {
				t.Errorf("reason %q does not mention health status %q", reason, tc.health)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Fail-closed paths — every uncertainty is a graceful skip, never a crash.
// -----------------------------------------------------------------------------

// TestAllow_FailClosed_EmptyResources verifies that a Synced+Healthy Application
// that nonetheless manages zero live workloads cannot be tied to the target, so
// the gate fails closed. With the authoritative resource-inventory mapper, an
// Application managing no live workload manifests as an unresolved governing
// Application (the defensive "manages no live resources" branch is unreachable
// through the API, because a successful resolution already implies a live
// workload).
func TestAllow_FailClosed_EmptyResources(t *testing.T) {
	srv := appServer(t, http.StatusOK, SyncStatusSynced, HealthStatusHealthy, 0)
	defer srv.Close()

	p := newArgoPrecheck(t, srv)
	allowed, reason, err := p.Allow(nil, standardTarget())
	assertGracefulSkip(t, allowed, reason, err)
	if !contains(reason, "resolve") {
		t.Errorf("reason %q, want it to mention the unresolved governing Application", reason)
	}
}

// TestAllow_FailClosed_NotFound verifies that a 404 from the Argo CD API (the
// Application was deleted or renamed) is a graceful skip.
func TestAllow_FailClosed_NotFound(t *testing.T) {
	srv := appServer(t, http.StatusNotFound, SyncStatusSynced, HealthStatusHealthy, 1)
	defer srv.Close()

	p := newArgoPrecheck(t, srv)
	allowed, reason, err := p.Allow(nil, standardTarget())
	assertGracefulSkip(t, allowed, reason, err)
	if !contains(reason, "resolve") {
		t.Errorf("reason %q, want it to mention the unresolved governing Application", reason)
	}
}

// TestAllow_FailClosed_Forbidden verifies that a 403 (bad token, or the
// Application is in a different project) is a graceful skip.
func TestAllow_FailClosed_Forbidden(t *testing.T) {
	srv := appServer(t, http.StatusForbidden, SyncStatusSynced, HealthStatusHealthy, 1)
	defer srv.Close()

	p := newArgoPrecheck(t, srv)
	allowed, reason, err := p.Allow(nil, standardTarget())
	assertGracefulSkip(t, allowed, reason, err)
	if !contains(reason, "resolve") {
		t.Errorf("reason %q, want it to mention the unresolved governing Application", reason)
	}
}

// TestAllow_FailClosed_ServerError verifies that a 5xx from the Argo CD API is a
// graceful skip rather than an error that could crash the scheduler.
func TestAllow_FailClosed_ServerError(t *testing.T) {
	srv := appServer(t, http.StatusInternalServerError, SyncStatusSynced, HealthStatusHealthy, 1)
	defer srv.Close()

	p := newArgoPrecheck(t, srv)
	allowed, reason, err := p.Allow(nil, standardTarget())
	assertGracefulSkip(t, allowed, reason, err)
	if !contains(reason, "resolve") {
		t.Errorf("reason %q, want it to mention the unresolved governing Application", reason)
	}
}

// TestAllow_FailClosed_Unreachable verifies that a connection failure (the
// server is closed before the call, so the URL refuses connections) is
// converted into a graceful skip. The precheck is constructed from the live URL
// first, then the server is closed so the client points at a dead address.
func TestAllow_FailClosed_Unreachable(t *testing.T) {
	srv := appServer(t, http.StatusOK, SyncStatusSynced, HealthStatusHealthy, 1)
	p := newArgoPrecheck(t, srv)
	srv.Close() // now unreachable; the pending GET will fail to connect.

	allowed, reason, err := p.Allow(nil, standardTarget())
	assertGracefulSkip(t, allowed, reason, err)
}

// TestAllow_FailClosed_Unresolvable verifies that when the eligible Application
// is reachable and healthy but the single live workload it manages belongs to a
// DIFFERENT target, the selected instance cannot be tied to any eligible
// Application, so the gate fails closed. No name-coincidence shortcut is taken.
func TestAllow_FailClosed_Unresolvable(t *testing.T) {
	// Application "myapp" (eligible) manages one live Deployment named
	// "someone-else", which does not match the standard target's identifiers.
	const unrelated = `{"metadata":{"name":"myapp"},"status":{` +
		`"sync":{"status":"Synced"},"health":{"status":"Healthy"},` +
		`"resources":[{"group":"apps","kind":"Deployment","name":"someone-else",` +
		`"health":{"status":"Healthy"}}]}}`
	srv := newServer(http.StatusOK, unrelated, http.StatusOK, nil)
	defer srv.Close()

	p := newArgoPrecheck(t, srv)
	allowed, reason, err := p.Allow(nil, standardTarget())
	assertGracefulSkip(t, allowed, reason, err)
	if !contains(reason, "resolve") {
		t.Errorf("reason %q, want it to mention the unresolved governing Application", reason)
	}
}

// TestAllow_FailClosed_NoEligibleApplications verifies that an empty eligibility
// allow-list authorizes nothing: the gate denies before making any API call,
// even against a reachable, healthy server.
func TestAllow_FailClosed_NoEligibleApplications(t *testing.T) {
	srv := appServer(t, http.StatusOK, SyncStatusSynced, HealthStatusHealthy, 1)
	defer srv.Close()

	p := &argoPrecheck{
		client:  mustClient(t, srv.URL, nil),
		mapper:  NewMapper(nil), // no eligible Applications configured
		timeout: 2 * time.Second,
	}
	allowed, reason, err := p.Allow(nil, standardTarget())
	assertGracefulSkip(t, allowed, reason, err)
	if !contains(reason, "no eligible") {
		t.Errorf("reason %q, want it to mention that no eligible Applications are configured", reason)
	}
}

// -----------------------------------------------------------------------------
// Inert when disabled + factory wiring (GetPrecheck).
// -----------------------------------------------------------------------------

// TestGetPrecheck_DisabledReturnsAllowAll verifies that when the integration is
// disabled (the default), GetPrecheck returns the inert allow-all provider that
// permits every termination with no error and an empty reason, preserving
// existing behavior byte-for-byte.
func TestGetPrecheck_DisabledReturnsAllowAll(t *testing.T) {
	m := config.Defaults() // argocd.enabled defaults to false
	p, err := GetPrecheck(m)
	if err != nil {
		t.Fatalf("GetPrecheck(disabled) error = %v, want nil", err)
	}
	if _, ok := p.(allowAllPrecheck); !ok {
		t.Errorf("GetPrecheck(disabled) returned %T, want allowAllPrecheck", p)
	}
	allowed, reason, err := p.Allow(nil, mock.Instance{App: "foo"})
	if err != nil {
		t.Fatalf("allow-all Allow error = %v, want nil", err)
	}
	if !allowed {
		t.Error("allow-all Allow allowed = false, want true (the feature must be inert when disabled)")
	}
	if reason != "" {
		t.Errorf("allow-all Allow reason = %q, want empty", reason)
	}
}

// TestGetPrecheck_EnabledNoEndpoint_Errors verifies that genuine misconfiguration
// surfaces at construction: an enabled integration with a credential but no
// endpoint cannot form a valid client, so GetPrecheck returns an error (rather
// than a silently broken provider).
func TestGetPrecheck_EnabledNoEndpoint_Errors(t *testing.T) {
	m := config.Defaults()
	m.Set(param.ArgoCDEnabled, true)
	// A token is supplied so the config snapshot itself is valid; the missing
	// endpoint is the misconfiguration GetPrecheck must reject.
	m.Set(param.ArgoCDToken, "t")
	if _, err := GetPrecheck(m); err == nil {
		t.Error("GetPrecheck(enabled, no endpoint) error = nil, want a construction error")
	}
}

// TestGetPrecheck_EnabledEndToEnd_Allows validates the full factory -> client ->
// gate wiring: a complete enabled configuration pointed at a Synced+Healthy mock
// Application allows the termination.
func TestGetPrecheck_EnabledEndToEnd_Allows(t *testing.T) {
	srv := appServer(t, http.StatusOK, SyncStatusSynced, HealthStatusHealthy, 1)
	defer srv.Close()

	m := config.Defaults()
	m.Set(param.ArgoCDEnabled, true)
	m.Set(param.ArgoCDEndpoint, srv.URL)
	m.Set(param.ArgoCDToken, "t")
	// The eligible allow-list is mandatory: without it the mapper authorizes
	// nothing and the gate would deny even a healthy Application.
	m.Set(param.ArgoCDApplications, []string{precheckApp})

	p, err := GetPrecheck(m)
	if err != nil {
		t.Fatalf("GetPrecheck(enabled) error = %v, want nil", err)
	}
	allowed, reason, err := p.Allow(nil, standardTarget())
	if err != nil {
		t.Fatalf("Allow error = %v, want nil", err)
	}
	if !allowed {
		t.Errorf("Allow allowed = false, want true (full factory->client->gate wiring); reason=%q", reason)
	}
}
