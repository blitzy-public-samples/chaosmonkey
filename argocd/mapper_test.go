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

// White-box, table-driven safety tests for mapper.go and the resource-level
// predicates in application.go. They assert the fail-closed ownership contract
// that Capability A depends on:
//
//   - an empty eligibility allow-list authorizes NOTHING;
//   - eligibility is exact-name only (with normalization);
//   - a target is bound to an Application only when that Application is eligible
//     AND its status.resources[] contains exactly one live workload resource
//     whose name matches the target — never by bare Application-name coincidence;
//   - nil/typed-nil/ambiguous/ineligible/non-live inputs all deny without panic.
//
// The tests use only the standard-library testing package with a table-driven
// style, matching every other test in this module (no test file imports testify;
// it is an indirect-only go.mod entry whose direct use would force a
// go.mod/go.sum change the minimal-change contract forbids).

import (
	"context"
	"errors"
	"testing"

	"github.com/Netflix/chaosmonkey/v2"
	"github.com/Netflix/chaosmonkey/v2/grp"
	"github.com/Netflix/chaosmonkey/v2/mock"
)

// nilableInstance embeds the Instance interface so that a (*nilableInstance)(nil)
// value satisfies chaosmonkey.Instance yet is a typed-nil whose promoted methods
// would panic if invoked. It proves ManagedResourceFor guards typed-nil inputs.
type nilableInstance struct{ chaosmonkey.Instance }

// healthy builds a live workload ResourceStatus (health reported and Healthy).
func healthy(group, kind, name string) ResourceStatus {
	return ResourceStatus{Group: group, Kind: kind, Name: name, Health: &HealthStatus{Status: HealthStatusHealthy}}
}

// withHealth builds a workload ResourceStatus with an explicit health string.
func withHealth(group, kind, name, health string) ResourceStatus {
	return ResourceStatus{Group: group, Kind: kind, Name: name, Health: &HealthStatus{Status: health}}
}

// appWith builds an *Application with the given metadata name and resources.
func appWith(name string, resources ...ResourceStatus) *Application {
	a := &Application{}
	a.Metadata.Name = name
	a.Status.Resources = resources
	return a
}

// -----------------------------------------------------------------------------
// Eligibility — the operator allow-list is the sole source of eligibility.
// -----------------------------------------------------------------------------

// TestEligibility covers NewMapper normalization plus IsEligible / an empty
// allow-list denying everything (the C-01 fix).
func TestEligibility(t *testing.T) {
	cases := []struct {
		name       string
		configured []string
		query      string
		wantElig   bool
	}{
		{"empty list denies any name", nil, "foo", false},
		{"empty list denies empty query", nil, "", false},
		{"exact match is eligible", []string{"foo", "bar"}, "foo", true},
		{"non-member is ineligible", []string{"foo", "bar"}, "baz", false},
		{"empty query is never eligible", []string{"foo"}, "", false},
		{"whitespace in configured name is trimmed", []string{"  foo  "}, "foo", true},
		{"whitespace in query is trimmed", []string{"foo"}, "  foo  ", true},
		{"match is case-sensitive", []string{"Foo"}, "foo", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMapper(tc.configured)
			if got := m.IsEligible(tc.query); got != tc.wantElig {
				t.Errorf("IsEligible(%q) with %v = %v, want %v", tc.query, tc.configured, got, tc.wantElig)
			}
		})
	}
}

// TestNewMapper_NormalizesAndDeduplicates verifies whitespace trimming, empty
// dropping, de-duplication, and order preservation (the m-04/M-02 fix).
func TestNewMapper_NormalizesAndDeduplicates(t *testing.T) {
	m := NewMapper([]string{"  foo ", "", "bar", "foo", "   ", "bar", "baz"})
	got := m.EligibleApplications()
	want := []string{"foo", "bar", "baz"}
	if len(got) != len(want) {
		t.Fatalf("EligibleApplications() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("EligibleApplications()[%d] = %q, want %q (order must be preserved)", i, got[i], want[i])
		}
	}
}

// TestEligibleApplications_ReturnsCopy verifies the accessor returns a copy so a
// caller cannot mutate the Mapper's internal state (the m-04 aliasing fix).
func TestEligibleApplications_ReturnsCopy(t *testing.T) {
	m := NewMapper([]string{"foo", "bar"})
	got := m.EligibleApplications()
	got[0] = "mutated"
	if !m.IsEligible("foo") || m.IsEligible("mutated") {
		t.Errorf("mutating the returned slice changed Mapper state: IsEligible(foo)=%v IsEligible(mutated)=%v",
			m.IsEligible("foo"), m.IsEligible("mutated"))
	}
}

// TestNewMapper_DoesNotAliasInput verifies the constructor copies its input so
// later caller mutation cannot change eligibility (the m-04 aliasing fix).
func TestNewMapper_DoesNotAliasInput(t *testing.T) {
	in := []string{"foo", "bar"}
	m := NewMapper(in)
	in[0] = "mutated"
	if !m.IsEligible("foo") || m.IsEligible("mutated") {
		t.Errorf("mutating the input slice changed Mapper state: IsEligible(foo)=%v IsEligible(mutated)=%v",
			m.IsEligible("foo"), m.IsEligible("mutated"))
	}
}

// TestEmptyAllowList_ManagedResourceForDenies is the headline C-01 safety
// assertion: with no eligible Applications configured, even a perfectly
// matching, healthy, live workload is denied.
func TestEmptyAllowList_ManagedResourceForDenies(t *testing.T) {
	m := NewMapper(nil)
	app := appWith("foo", healthy("apps", "Deployment", "foo"))
	ins := mock.Instance{App: "foo", Cluster: "foo"}
	if _, ok := m.ManagedResourceFor(app, nil, ins); ok {
		t.Error("empty allow-list must deny every target (fail-closed), got ok=true")
	}
}

// -----------------------------------------------------------------------------
// ManagedResourceFor — authoritative, fail-closed ownership correlation.
// -----------------------------------------------------------------------------

func TestManagedResourceFor(t *testing.T) {
	var nilInstance chaosmonkey.Instance
	typedNil := chaosmonkey.Instance((*nilableInstance)(nil))

	cases := []struct {
		name       string
		configured []string
		app        *Application
		group      grp.InstanceGroup
		instance   chaosmonkey.Instance
		wantOK     bool
		wantName   string
	}{
		{
			name:       "matching live Deployment by cluster name allows",
			configured: []string{"foo"},
			app:        appWith("foo", healthy("apps", "Deployment", "foo")),
			instance:   mock.Instance{Cluster: "foo"},
			wantOK:     true,
			wantName:   "foo",
		},
		{
			name:       "matching live Deployment by app name allows",
			configured: []string{"foo"},
			app:        appWith("foo", healthy("apps", "Deployment", "foo")),
			instance:   mock.Instance{App: "foo", Cluster: ""},
			wantOK:     true,
			wantName:   "foo",
		},
		{
			name:       "same-named but ineligible Application denies",
			configured: []string{"bar"},
			app:        appWith("foo", healthy("apps", "Deployment", "foo")),
			instance:   mock.Instance{Cluster: "foo"},
			wantOK:     false,
		},
		{
			name:       "eligible Application not managing the target denies",
			configured: []string{"foo"},
			app:        appWith("foo", healthy("apps", "Deployment", "other")),
			instance:   mock.Instance{Cluster: "foo"},
			wantOK:     false,
		},
		{
			name:       "only non-workload resources denies",
			configured: []string{"foo"},
			app:        appWith("foo", healthy("", "Service", "foo"), healthy("", "ConfigMap", "foo")),
			instance:   mock.Instance{Cluster: "foo"},
			wantOK:     false,
		},
		{
			name:       "workload reported Missing denies",
			configured: []string{"foo"},
			app:        appWith("foo", withHealth("apps", "Deployment", "foo", HealthStatusMissing)),
			instance:   mock.Instance{Cluster: "foo"},
			wantOK:     false,
		},
		{
			name:       "workload with absent health denies",
			configured: []string{"foo"},
			app:        appWith("foo", ResourceStatus{Group: "apps", Kind: "Deployment", Name: "foo"}),
			instance:   mock.Instance{Cluster: "foo"},
			wantOK:     false,
		},
		{
			name:       "workload kind in wrong API group denies",
			configured: []string{"foo"},
			app:        appWith("foo", healthy("networking.k8s.io", "Deployment", "foo")),
			instance:   mock.Instance{Cluster: "foo"},
			wantOK:     false,
		},
		{
			name:       "ambiguous multiple matching live workloads denies",
			configured: []string{"foo"},
			app:        appWith("foo", healthy("apps", "Deployment", "foo"), healthy("apps", "StatefulSet", "foo")),
			instance:   mock.Instance{Cluster: "foo"},
			wantOK:     false,
		},
		{
			name:       "match via group app name allows",
			configured: []string{"bar"},
			app:        appWith("bar", healthy("apps", "Deployment", "bar")),
			group:      grp.New("bar", "prod", "us-east-1", "", ""),
			instance:   mock.Instance{},
			wantOK:     true,
			wantName:   "bar",
		},
		{
			name:       "match via group cluster name allows",
			configured: []string{"app1"},
			app:        appWith("app1", healthy("apps", "StatefulSet", "app1-prod")),
			group:      grp.New("app1", "prod", "us-east-1", "", "app1-prod"),
			instance:   mock.Instance{},
			wantOK:     true,
			wantName:   "app1-prod",
		},
		{
			name:       "nil Application denies",
			configured: []string{"foo"},
			app:        nil,
			instance:   mock.Instance{Cluster: "foo"},
			wantOK:     false,
		},
		{
			name:       "nil instance denies",
			configured: []string{"foo"},
			app:        appWith("foo", healthy("apps", "Deployment", "foo")),
			instance:   nilInstance,
			wantOK:     false,
		},
		{
			name:       "typed-nil instance denies without panic",
			configured: []string{"foo"},
			app:        appWith("foo", healthy("apps", "Deployment", "foo")),
			instance:   typedNil,
			wantOK:     false,
		},
		{
			name:       "empty identifiers with nil group denies",
			configured: []string{"foo"},
			app:        appWith("foo", healthy("apps", "Deployment", "foo")),
			instance:   mock.Instance{},
			wantOK:     false,
		},
		{
			name:       "live Pod in core group allows",
			configured: []string{"foo"},
			app:        appWith("foo", healthy("", "Pod", "foo")),
			instance:   mock.Instance{Cluster: "foo"},
			wantOK:     true,
			wantName:   "foo",
		},
		{
			name:       "live Rollout in argoproj.io group allows",
			configured: []string{"foo"},
			app:        appWith("foo", healthy("argoproj.io", "Rollout", "foo")),
			instance:   mock.Instance{Cluster: "foo"},
			wantOK:     true,
			wantName:   "foo",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMapper(tc.configured)
			got, ok := m.ManagedResourceFor(tc.app, tc.group, tc.instance)
			if ok != tc.wantOK {
				t.Fatalf("ManagedResourceFor ok = %v, want %v (resource=%+v)", ok, tc.wantOK, got)
			}
			if ok && got.Name != tc.wantName {
				t.Errorf("ManagedResourceFor resource name = %q, want %q", got.Name, tc.wantName)
			}
			if !ok && got.Name != "" {
				t.Errorf("denied result must be the zero ResourceStatus, got name=%q", got.Name)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// application.go predicates — nil-safety (M-04) and live-workload semantics (M-01)
// -----------------------------------------------------------------------------

// TestApplicationPredicates_NilReceiver verifies the *Application predicates are
// nil-safe and fail closed (return false / nil) rather than panicking.
func TestApplicationPredicates_NilReceiver(t *testing.T) {
	var a *Application
	if a.IsSynced() {
		t.Error("nil *Application IsSynced() = true, want false")
	}
	if a.IsHealthy() {
		t.Error("nil *Application IsHealthy() = true, want false")
	}
	if a.HasLiveResources() {
		t.Error("nil *Application HasLiveResources() = true, want false")
	}
	if got := a.LiveWorkloads(); got != nil {
		t.Errorf("nil *Application LiveWorkloads() = %v, want nil", got)
	}
}

// TestIsSyncedIsHealthy_ExactEquality verifies only the exact Synced / Healthy
// strings pass, so unknown/future values fail closed.
func TestIsSyncedIsHealthy_ExactEquality(t *testing.T) {
	cases := []struct {
		sync, health           string
		wantSynced, wantHealth bool
	}{
		{"Synced", "Healthy", true, true},
		{"OutOfSync", "Degraded", false, false},
		{"Synced", "Progressing", true, false},
		{"", "", false, false},
		{"Unknown", "Unknown", false, false},
		{"synced", "healthy", false, false}, // case-sensitive
	}
	for _, tc := range cases {
		a := &Application{}
		a.Status.Sync.Status = tc.sync
		a.Status.Health.Status = tc.health
		if got := a.IsSynced(); got != tc.wantSynced {
			t.Errorf("IsSynced(sync=%q) = %v, want %v", tc.sync, got, tc.wantSynced)
		}
		if got := a.IsHealthy(); got != tc.wantHealth {
			t.Errorf("IsHealthy(health=%q) = %v, want %v", tc.health, got, tc.wantHealth)
		}
	}
}

// TestIsLiveWorkload verifies the GVK + liveness predicate. Liveness uses a
// fail-closed ALLOW-LIST of known-live health states (Healthy / Progressing /
// Degraded / Suspended); an empty or unrecognized/future health is NOT live.
// Sync Hooks and prune-pending resources are transient GitOps artifacts and are
// excluded from ownership even when otherwise live.
// (GVK + liveness added per M-01; allow-list + Hook/RequiresPruning exclusion
// added per code review C-02.)
func TestIsLiveWorkload(t *testing.T) {
	healthPtr := func(s string) *HealthStatus { return &HealthStatus{Status: s} }
	cases := []struct {
		name string
		r    ResourceStatus
		want bool
	}{
		{"healthy Deployment in apps", ResourceStatus{Group: "apps", Kind: "Deployment", Health: healthPtr(HealthStatusHealthy)}, true},
		{"healthy Pod in core", ResourceStatus{Group: "", Kind: "Pod", Health: healthPtr(HealthStatusHealthy)}, true},
		{"healthy Rollout in argoproj.io", ResourceStatus{Group: "argoproj.io", Kind: "Rollout", Health: healthPtr(HealthStatusHealthy)}, true},
		{"progressing Deployment is still live", ResourceStatus{Group: "apps", Kind: "Deployment", Health: healthPtr(HealthStatusProgressing)}, true},
		{"degraded Deployment is still live", ResourceStatus{Group: "apps", Kind: "Deployment", Health: healthPtr(HealthStatusDegraded)}, true},
		{"suspended Deployment is still live", ResourceStatus{Group: "apps", Kind: "Deployment", Health: healthPtr(HealthStatusSuspended)}, true},
		{"missing Deployment is not live", ResourceStatus{Group: "apps", Kind: "Deployment", Health: healthPtr(HealthStatusMissing)}, false},
		{"unknown Deployment is not live", ResourceStatus{Group: "apps", Kind: "Deployment", Health: healthPtr(HealthStatusUnknown)}, false},
		{"empty health is not live", ResourceStatus{Group: "apps", Kind: "Deployment", Health: healthPtr("")}, false},
		{"unrecognized future health is not live", ResourceStatus{Group: "apps", Kind: "Deployment", Health: healthPtr("Frobnicated")}, false},
		{"absent health is not live", ResourceStatus{Group: "apps", Kind: "Deployment"}, false},
		{"hook resource is excluded even if healthy", ResourceStatus{Group: "apps", Kind: "Deployment", Health: healthPtr(HealthStatusHealthy), Hook: true}, false},
		{"prune-pending resource is excluded even if healthy", ResourceStatus{Group: "apps", Kind: "Deployment", Health: healthPtr(HealthStatusHealthy), RequiresPruning: true}, false},
		{"Service is not a workload", ResourceStatus{Group: "", Kind: "Service", Health: healthPtr(HealthStatusHealthy)}, false},
		{"ConfigMap is not a workload", ResourceStatus{Group: "", Kind: "ConfigMap", Health: healthPtr(HealthStatusHealthy)}, false},
		{"Deployment in wrong group is not a workload", ResourceStatus{Group: "extensions", Kind: "Deployment", Health: healthPtr(HealthStatusHealthy)}, false},
		{"Pod in non-core group is not a workload", ResourceStatus{Group: "apps", Kind: "Pod", Health: healthPtr(HealthStatusHealthy)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.IsLiveWorkload(); got != tc.want {
				t.Errorf("IsLiveWorkload() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHasLiveResourcesAndLiveWorkloads verifies HasLiveResources requires a live
// workload (not just any resource) and LiveWorkloads preserves order (M-01).
func TestHasLiveResourcesAndLiveWorkloads(t *testing.T) {
	app := appWith("x",
		healthy("", "Service", "svc"),                                // not a workload
		withHealth("apps", "Deployment", "web", HealthStatusMissing), // workload but not live
		healthy("apps", "StatefulSet", "db"),                         // live workload
		healthy("", "Pod", "web-abc"),                                // live workload
	)
	if !app.HasLiveResources() {
		t.Error("HasLiveResources() = false, want true (app has live workloads)")
	}
	live := app.LiveWorkloads()
	if len(live) != 2 {
		t.Fatalf("LiveWorkloads() len = %d, want 2", len(live))
	}
	if live[0].Name != "db" || live[1].Name != "web-abc" {
		t.Errorf("LiveWorkloads() = [%q,%q], want [db,web-abc] (order preserved)", live[0].Name, live[1].Name)
	}

	only := appWith("y", healthy("", "Service", "svc"), withHealth("apps", "Deployment", "web", HealthStatusMissing))
	if only.HasLiveResources() {
		t.Error("HasLiveResources() = true for Service-only + Missing workload, want false")
	}
}

// fakeGetter is an in-memory applicationGetter for unit-testing the shared
// resolver without a live REST client. A name present in errs returns that
// error; a name present in apps returns that Application; any other name returns
// a not-found error. (Added for the Argo CD integration per code review C-03/C-04.)
type fakeGetter struct {
	apps map[string]*Application
	errs map[string]error
}

func (f fakeGetter) GetApplication(_ context.Context, name string) (*Application, error) {
	if err, ok := f.errs[name]; ok {
		return nil, err
	}
	if app, ok := f.apps[name]; ok {
		return app, nil
	}
	return nil, errors.New("not found: " + name)
}

// TestResolveGoverningApplication exercises the shared, fail-closed resolver's
// full decision matrix in isolation (C-03/C-04): unique match, cross-Application
// ambiguity, one match with an unevaluated candidate (unprovable uniqueness),
// returned-name mismatch, a lookup error with no match, a clean no-match, and an
// empty eligibility allow-list. Every non-unique outcome must yield a nil result
// and a descriptive error so callers fail closed.
func TestResolveGoverningApplication(t *testing.T) {
	target := mock.Instance{App: "web", Cluster: "web"}
	ctx := context.Background()

	t.Run("unique match returns immutable identity", func(t *testing.T) {
		appA := appWith("appA", healthy("apps", "Deployment", "web"))
		appA.Metadata.Namespace = "argocd"
		m := NewMapper([]string{"appA", "appB"})
		g := fakeGetter{apps: map[string]*Application{
			"appA": appA,
			"appB": appWith("appB", healthy("apps", "Deployment", "other")),
		}}
		res, err := m.resolveGoverningApplication(ctx, g, nil, target)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if res == nil || res.Name() != "appA" {
			t.Fatalf("resolved = %v, want the appA identity", res)
		}
		if res.Namespace() != "argocd" {
			t.Errorf("Namespace() = %q, want %q", res.Namespace(), "argocd")
		}
		if res.Resource().Name != "web" {
			t.Errorf("Resource().Name = %q, want %q", res.Resource().Name, "web")
		}
	})

	t.Run("cross-application ambiguity denies", func(t *testing.T) {
		m := NewMapper([]string{"appA", "appB"})
		g := fakeGetter{apps: map[string]*Application{
			"appA": appWith("appA", healthy("apps", "Deployment", "web")),
			"appB": appWith("appB", healthy("apps", "Deployment", "web")),
		}}
		res, err := m.resolveGoverningApplication(ctx, g, nil, target)
		if res != nil || err == nil {
			t.Fatalf("res=%v err=%v, want nil result + ambiguity error", res, err)
		}
		if !contains(err.Error(), "ambiguous") {
			t.Errorf("err = %q, want it to report ambiguous ownership", err.Error())
		}
	})

	t.Run("one match with unevaluated candidate denies", func(t *testing.T) {
		m := NewMapper([]string{"appA", "appB"})
		g := fakeGetter{
			apps: map[string]*Application{"appA": appWith("appA", healthy("apps", "Deployment", "web"))},
			errs: map[string]error{"appB": errors.New("boom")},
		}
		res, err := m.resolveGoverningApplication(ctx, g, nil, target)
		if res != nil || err == nil {
			t.Fatalf("res=%v err=%v, want nil result + unprovable-uniqueness error", res, err)
		}
		if !contains(err.Error(), "unique ownership") {
			t.Errorf("err = %q, want it to report unprovable unique ownership", err.Error())
		}
	})

	t.Run("returned-name mismatch denies", func(t *testing.T) {
		m := NewMapper([]string{"appA"})
		g := fakeGetter{apps: map[string]*Application{
			"appA": appWith("someone-else", healthy("apps", "Deployment", "web")),
		}}
		res, err := m.resolveGoverningApplication(ctx, g, nil, target)
		if res != nil || err == nil {
			t.Fatalf("res=%v err=%v, want nil result + mismatch error", res, err)
		}
		if !contains(err.Error(), "different Application") {
			t.Errorf("err = %q, want it to report the returned-name mismatch", err.Error())
		}
	})

	t.Run("lookup error with no match surfaces error", func(t *testing.T) {
		m := NewMapper([]string{"appA"})
		g := fakeGetter{errs: map[string]error{"appA": errors.New("boom")}}
		res, err := m.resolveGoverningApplication(ctx, g, nil, target)
		if res != nil || err == nil {
			t.Fatalf("res=%v err=%v, want nil result + lookup error", res, err)
		}
		if !contains(err.Error(), "could not resolve") {
			t.Errorf("err = %q, want it to report an unresolved governing Application", err.Error())
		}
	})

	t.Run("clean no-match denies", func(t *testing.T) {
		m := NewMapper([]string{"appA"})
		g := fakeGetter{apps: map[string]*Application{
			"appA": appWith("appA", healthy("apps", "Deployment", "other")),
		}}
		res, err := m.resolveGoverningApplication(ctx, g, nil, target)
		if res != nil || err == nil {
			t.Fatalf("res=%v err=%v, want nil result + no-match error", res, err)
		}
		if !contains(err.Error(), "manages the target") {
			t.Errorf("err = %q, want it to report that no eligible Application manages the target", err.Error())
		}
	})

	t.Run("empty allow-list denies before any lookup", func(t *testing.T) {
		m := NewMapper(nil)
		res, err := m.resolveGoverningApplication(ctx, fakeGetter{}, nil, target)
		if res != nil || err == nil {
			t.Fatalf("res=%v err=%v, want nil result + no-eligible error", res, err)
		}
		if !contains(err.Error(), "no eligible Applications configured") {
			t.Errorf("err = %q, want it to report no eligible Applications configured", err.Error())
		}
	})
}
