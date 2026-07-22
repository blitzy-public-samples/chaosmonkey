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

// White-box unit tests for mapper.go. They exercise the instance-to-Application
// resolution rules (allow-list vs. fallback), the InstanceGroup-aware variant,
// and the ManagedWorkloads filtering of status.resources[]. The (name, ok)
// contract is the resolution-layer half of the integration's fail-closed
// guarantee: an unresolvable target must yield ok=false so precheck.go can turn
// it into a safe skip.
//
// These tests deliberately use the standard-library testing package only. The
// Argo CD integration is constrained to leave go.mod/go.sum untouched, and
// importing a third-party assertion library (e.g. testify) would require adding
// its transitive dependencies to go.mod. The plain-testing style also matches
// the rest of the repository's test suite (e.g. spinnaker/terminator_test.go).

import (
	"testing"

	"github.com/Netflix/chaosmonkey/v2/grp"
	"github.com/Netflix/chaosmonkey/v2/mock"
)

// -----------------------------------------------------------------------------
// Resolution — no allow-list (fallback path)
//
// With no configured Application allow-list, the first non-empty candidate is
// used directly as the Application name. Candidate order is cluster then app.
// -----------------------------------------------------------------------------

// TestApplicationForInstance_NoAllowList_ClusterWins verifies that, absent an
// allow-list, the instance's cluster name is preferred as the Application name.
func TestApplicationForInstance_NoAllowList_ClusterWins(t *testing.T) {
	m := NewMapper(nil)
	ins := mock.Instance{App: "foo", Cluster: "foo-beta"}

	name, ok := m.ApplicationForInstance(ins)

	if !ok {
		t.Fatalf("expected the target to resolve, got ok=false")
	}
	if got, want := name, "foo-beta"; got != want {
		t.Errorf("got name=%q, want %q (cluster is the first candidate)", got, want)
	}
}

// TestApplicationForInstance_NoAllowList_AppFallback verifies that the app name
// is used when the cluster name is empty.
func TestApplicationForInstance_NoAllowList_AppFallback(t *testing.T) {
	m := NewMapper(nil)
	ins := mock.Instance{App: "foo", Cluster: ""}

	name, ok := m.ApplicationForInstance(ins)

	if !ok {
		t.Fatalf("expected the target to resolve via the app fallback, got ok=false")
	}
	if got, want := name, "foo"; got != want {
		t.Errorf("got name=%q, want %q (app is used when cluster is empty)", got, want)
	}
}

// TestApplicationForInstance_NoAllowList_AllEmpty verifies the fail-closed
// contract: when every candidate is empty, resolution fails (ok=false).
func TestApplicationForInstance_NoAllowList_AllEmpty(t *testing.T) {
	m := NewMapper(nil)
	ins := mock.Instance{}

	name, ok := m.ApplicationForInstance(ins)

	if ok {
		t.Errorf("expected ok=false for an empty instance (fail-closed), got ok=true")
	}
	if name != "" {
		t.Errorf("got name=%q, want empty string when nothing resolves", name)
	}
}

// -----------------------------------------------------------------------------
// Resolution — with allow-list
//
// With a configured allow-list, only a candidate that exactly matches a
// configured Application name is accepted. Configured order breaks ties.
// -----------------------------------------------------------------------------

// TestApplicationForInstance_AllowList_Match verifies that a cluster candidate
// matching a configured Application name resolves to that name.
func TestApplicationForInstance_AllowList_Match(t *testing.T) {
	m := NewMapper([]string{"foo-beta", "bar"})
	ins := mock.Instance{App: "foo", Cluster: "foo-beta"}

	name, ok := m.ApplicationForInstance(ins)

	if !ok {
		t.Fatalf("expected the cluster candidate to match a configured Application, got ok=false")
	}
	if got, want := name, "foo-beta"; got != want {
		t.Errorf("got name=%q, want %q", got, want)
	}
}

// TestApplicationForInstance_AllowList_MatchByApp verifies that the app
// candidate resolves against the allow-list even when the cluster candidate
// does not match any configured name.
func TestApplicationForInstance_AllowList_MatchByApp(t *testing.T) {
	m := NewMapper([]string{"foo"})
	ins := mock.Instance{App: "foo", Cluster: "foo-beta"}

	name, ok := m.ApplicationForInstance(ins)

	if !ok {
		t.Fatalf("expected the app candidate to match the configured Application, got ok=false")
	}
	if got, want := name, "foo"; got != want {
		t.Errorf("got name=%q, want %q (cluster does not match, but app does)", got, want)
	}
}

// TestApplicationForInstance_AllowList_NoMatch verifies the fail-closed hinge:
// when no candidate matches any configured Application, resolution fails.
func TestApplicationForInstance_AllowList_NoMatch(t *testing.T) {
	m := NewMapper([]string{"other"})
	ins := mock.Instance{App: "foo", Cluster: "foo-beta"}

	name, ok := m.ApplicationForInstance(ins)

	if ok {
		t.Errorf("expected ok=false when no candidate matches the allow-list (fail-closed), got ok=true")
	}
	if name != "" {
		t.Errorf("got name=%q, want empty string on an allow-list miss", name)
	}
}

// -----------------------------------------------------------------------------
// Resolution — with InstanceGroup
//
// ApplicationFor additionally considers the group's app name as a candidate and
// must tolerate a nil group without panicking.
// -----------------------------------------------------------------------------

// TestApplicationFor_GroupAppMatches verifies that the group's app name is used
// as a resolution candidate when the instance's own identifiers are empty.
func TestApplicationFor_GroupAppMatches(t *testing.T) {
	m := NewMapper([]string{"bar"})
	ins := mock.Instance{App: "", Cluster: ""}
	g := grp.New("bar", "test", "us-east-1", "prod", "bar-prod")

	name, ok := m.ApplicationFor(g, ins)

	if !ok {
		t.Fatalf("expected the group.App() candidate to resolve against the allow-list, got ok=false")
	}
	if got, want := name, "bar"; got != want {
		t.Errorf("got name=%q, want %q", got, want)
	}
}

// TestApplicationFor_NilGroupTolerated verifies that a nil InstanceGroup is
// handled gracefully (no panic) and resolution falls back to the instance.
func TestApplicationFor_NilGroupTolerated(t *testing.T) {
	m := NewMapper(nil)
	ins := mock.Instance{Cluster: "foo-beta"}

	name, ok := m.ApplicationFor(nil, ins)

	if !ok {
		t.Fatalf("expected resolution to succeed via the instance's cluster, got ok=false")
	}
	if got, want := name, "foo-beta"; got != want {
		t.Errorf("got name=%q, want %q (nil group must not affect fallback resolution)", got, want)
	}
}

// -----------------------------------------------------------------------------
// ManagedWorkloads — filtering status.resources[] to workload kinds
//
// Only workload kinds (Pod, Deployment, ReplicaSet, StatefulSet, DaemonSet,
// Rollout) are returned; original order is preserved. An empty result backs the
// gate's "manages no live resources" deny branch.
// -----------------------------------------------------------------------------

// TestManagedWorkloads_FiltersWorkloadKinds verifies that only workload kinds
// are returned, in their original order, and that non-workload kinds (Service,
// ConfigMap) are dropped.
func TestManagedWorkloads_FiltersWorkloadKinds(t *testing.T) {
	app := &Application{}
	app.Status.Resources = []ResourceStatus{
		{Kind: "Deployment", Name: "web"},
		{Kind: "Pod", Name: "web-abc"},
		{Kind: "Service", Name: "web-svc"},
		{Kind: "ConfigMap", Name: "cfg"},
	}

	got := ManagedWorkloads(app)

	// Guard the index accesses below from panicking on an unexpected result.
	if len(got) != 2 {
		t.Fatalf("got len(workloads)=%d, want 2 (only the Deployment and Pod are workload kinds)", len(got))
	}
	if got[0].Kind != "Deployment" || got[0].Name != "web" {
		t.Errorf("got workloads[0]={Kind:%q, Name:%q}, want {Deployment, web} (order must be preserved)", got[0].Kind, got[0].Name)
	}
	if got[1].Kind != "Pod" || got[1].Name != "web-abc" {
		t.Errorf("got workloads[1]={Kind:%q, Name:%q}, want {Pod, web-abc} (order must be preserved)", got[1].Kind, got[1].Name)
	}
}

// TestManagedWorkloads_NilApp verifies that a nil Application yields nil.
func TestManagedWorkloads_NilApp(t *testing.T) {
	if got := ManagedWorkloads(nil); got != nil {
		t.Errorf("got %v, want nil for a nil Application", got)
	}
}

// TestManagedWorkloads_NoWorkloadKinds verifies that an Application managing
// only non-workload kinds yields an empty result (the deny signal for
// "manages no live resources").
func TestManagedWorkloads_NoWorkloadKinds(t *testing.T) {
	app := &Application{}
	app.Status.Resources = []ResourceStatus{
		{Kind: "Service", Name: "web-svc"},
		{Kind: "ConfigMap", Name: "cfg"},
	}

	got := ManagedWorkloads(app)

	if len(got) != 0 {
		t.Errorf("got len(workloads)=%d, want 0 (no workload kinds present)", len(got))
	}
}

// TestManagedWorkloads_AllWorkloadKinds confirms that every recognized workload
// kind passes the filter, in the order supplied.
func TestManagedWorkloads_AllWorkloadKinds(t *testing.T) {
	app := &Application{}
	app.Status.Resources = []ResourceStatus{
		{Kind: "Deployment", Name: "d"},
		{Kind: "StatefulSet", Name: "ss"},
		{Kind: "DaemonSet", Name: "ds"},
		{Kind: "ReplicaSet", Name: "rs"},
		{Kind: "Rollout", Name: "ro"},
		{Kind: "Pod", Name: "p"},
	}

	got := ManagedWorkloads(app)

	wantKinds := []string{"Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Rollout", "Pod"}
	if len(got) != len(wantKinds) {
		t.Fatalf("got len(workloads)=%d, want %d (all workload kinds must pass the filter)", len(got), len(wantKinds))
	}
	for i, want := range wantKinds {
		if got[i].Kind != want {
			t.Errorf("got workloads[%d].Kind=%q, want %q (workload order must be preserved)", i, got[i].Kind, want)
		}
	}
}
