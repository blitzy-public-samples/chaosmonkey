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
	"context"
	"reflect"
	"strings"

	"github.com/pkg/errors"

	"github.com/Netflix/chaosmonkey/v2"
	"github.com/Netflix/chaosmonkey/v2/grp"
)

// Mapper correlates a selected Chaos Monkey termination target with the Argo CD
// Application that authoritatively manages it. Eligibility is driven solely by
// the operator-configured allow-list of exact Application names
// (Config.Applications): an empty allow-list makes NO target eligible
// (fail-closed), and only an Application whose name exactly matches a configured
// entry is ever considered.
//
// Ownership is never inferred from name coincidence. A candidate Application is
// accepted for a target only after correlation against the Application's own
// status.resources[] shows exactly one live workload resource — a supported
// Kubernetes workload GVK that Argo CD reports as live — whose name matches one
// of the target's identifiers. Any ambiguity, nil/malformed input, ineligible
// Application, or absence of a matching live workload yields ok=false, which the
// sync/health gate converts into a safe skip.
//
// Resolution performs no live Kubernetes label lookups (honoring the
// standard-library-only constraint); instead it correlates the target against
// the authoritative resource inventory Argo CD already reports on the
// Application. The (resource, ok) contract is the fail-closed hinge for the gate.
type Mapper struct {
	// applications is a normalized, de-duplicated copy of the configured
	// eligible Application names. It is a copy so that neither the caller nor
	// same-package code can mutate the Mapper's state after construction.
	applications []string
}

// NewMapper builds a Mapper from the configured eligible Application names. The
// input slice is copied and normalized (whitespace-trimmed, empty entries
// dropped, duplicates removed) so the Mapper never aliases caller-owned storage.
// (Copy/normalize added for the Argo CD integration per code review m-04/M-02.)
func NewMapper(applications []string) *Mapper {
	return &Mapper{applications: normalizeApplications(applications)}
}

// normalizeApplications returns a whitespace-trimmed, empty-dropped,
// order-preserving, de-duplicated copy of the supplied names. It never returns
// a slice that aliases the input.
func normalizeApplications(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, name := range in {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// EligibleApplications returns a copy of the configured eligible Application
// names. An empty result means NO Application is chaos-eligible (fail-closed):
// operators must explicitly opt Applications in via argocd.applications. The
// returned slice is a copy so callers cannot mutate the Mapper's state.
func (m *Mapper) EligibleApplications() []string {
	if len(m.applications) == 0 {
		return nil
	}
	out := make([]string, len(m.applications))
	copy(out, m.applications)
	return out
}

// IsEligible reports whether name exactly matches one of the configured
// eligible Application names. It is always false when no Application names are
// configured, so an empty allow-list denies every target (fail-closed).
func (m *Mapper) IsEligible(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, app := range m.applications {
		if app == name {
			return true
		}
	}
	return false
}

// isNilInstance reports whether an Instance interface value is nil or wraps a
// nil pointer (typed-nil). Calling methods on a typed-nil Instance would panic;
// guarding here lets ManagedResourceFor fail closed instead of crashing the
// scheduler. (Added for the Argo CD integration per code review M-04.)
func isNilInstance(instance chaosmonkey.Instance) bool {
	if instance == nil {
		return true
	}
	v := reflect.ValueOf(instance)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// targetIdentifiers returns the non-empty, de-duplicated set of names that may
// legitimately correspond to the Kubernetes workload name of the selected
// target: the instance's cluster and app names, plus (when a group is provided)
// the group's app and, if present, its cluster. These Spinnaker-side names are
// the pragmatic bridge to a managed workload's resource name; individual pod
// names (which carry random suffixes) are intentionally never fabricated here.
func targetIdentifiers(group grp.InstanceGroup, instance chaosmonkey.Instance) map[string]bool {
	ids := make(map[string]bool, 4)
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			ids[s] = true
		}
	}
	add(instance.ClusterName())
	add(instance.AppName())
	if group != nil {
		add(group.App())
		if cluster, ok := group.Cluster(); ok {
			add(cluster)
		}
	}
	return ids
}

// ManagedResourceFor returns the single live workload resource in app that
// authoritatively corresponds to the selected target, together with ok=true.
// It is the fail-closed core of Capability A. It returns (ResourceStatus{},
// false) — a deny — unless ALL of the following hold:
//
//   - instance is non-nil and not a typed-nil (guarded to avoid a panic);
//   - app is non-nil and its metadata.name is an eligible Application name
//     (IsEligible); a same-named but unlisted Application is rejected;
//   - exactly one resource in app.status.resources[] is a live workload
//     (IsLiveWorkload — a supported workload GVK that Argo CD reports as live)
//     whose name matches one of the target's identifiers.
//
// Zero matches (the target is not a managed live workload of this Application)
// and more than one match (ambiguous ownership) both deny. This binds the
// decision to Argo CD's authoritative status.resources[] inventory rather than
// to a bare Application-name coincidence.
// (Added for the Argo CD integration per code review C-01/M-01/M-04.)
func (m *Mapper) ManagedResourceFor(app *Application, group grp.InstanceGroup, instance chaosmonkey.Instance) (ResourceStatus, bool) {
	if app == nil || isNilInstance(instance) {
		return ResourceStatus{}, false
	}
	if !m.IsEligible(app.Metadata.Name) {
		return ResourceStatus{}, false
	}

	ids := targetIdentifiers(group, instance)
	if len(ids) == 0 {
		return ResourceStatus{}, false
	}

	var match ResourceStatus
	found := 0
	for _, r := range app.LiveWorkloads() {
		if ids[strings.TrimSpace(r.Name)] {
			match = r
			found++
		}
	}
	if found != 1 {
		// Zero matches => the target is not a managed live workload here.
		// More than one match => ambiguous ownership. Both fail closed.
		return ResourceStatus{}, false
	}
	return match, true
}

// applicationGetter is the read-only subset of the Argo CD client the resolver
// needs: a single Application lookup by name. *Client satisfies it. Declaring it
// as an interface keeps the Mapper decoupled from the concrete REST client and
// lets resolveGoverningApplication be unit-tested with a fake getter.
// (Added for the Argo CD integration per code review C-03/C-04.)
type applicationGetter interface {
	GetApplication(ctx context.Context, name string) (*Application, error)
}

// ResolvedApplication is the immutable outcome of a successful, globally unique
// target-to-Application resolution: the single governing Application, the exact
// live workload resource that ties the target to it, and the confirmed
// Application name. Fields are unexported and read via accessors so a resolved
// identity cannot be mutated after resolution.
// (Added for the Argo CD integration per code review C-03/C-04.)
type ResolvedApplication struct {
	app      *Application
	resource ResourceStatus
	name     string
}

// Name returns the confirmed governing Application name (the name requested from
// and echoed back by the Argo CD API).
func (r *ResolvedApplication) Name() string { return r.name }

// Namespace returns the governing Application's own metadata.namespace (the
// Argo CD control-plane namespace the Application object lives in).
func (r *ResolvedApplication) Namespace() string { return r.app.Metadata.Namespace }

// Application returns the governing Application.
func (r *ResolvedApplication) Application() *Application { return r.app }

// Resource returns the single live workload resource that tied the target to the
// governing Application.
func (r *ResolvedApplication) Resource() ResourceStatus { return r.resource }

// resolveGoverningApplication resolves the selected target to the ONE Argo CD
// Application that governs it, proving GLOBAL uniqueness across the entire
// operator-configured eligible set. It is the shared, fail-closed resolver used
// by both the sync/health gate (precheck.go) and the write-back tracker
// (tracker.go), so the two can never disagree about which Application a target
// belongs to. (Added for the Argo CD integration per code review C-03/C-04.)
//
// Unlike a first-match loop, it evaluates EVERY eligible Application (no early
// break) and only returns a resolution when exactly one Application manages the
// target AND every eligible Application was successfully evaluated. Specifically:
//
//   - It fetches each eligible Application. A fetch error, or a response whose
//     metadata.name does not equal the requested name (a rename/redirect/
//     misrouting), is recorded as an evaluation failure for that candidate and
//     the loop continues — one bad Application never masks another.
//   - >1 match  => ambiguous ownership: deny (error). Acting would risk
//     terminating a workload governed by a different Application than intended.
//   - exactly 1 match but some candidates failed to evaluate => uniqueness
//     cannot be proven (an unevaluated Application might also manage the target):
//     deny (error).
//   - exactly 1 match and every candidate evaluated cleanly => the unique
//     governing Application (allow to proceed to the sync/health check).
//   - 0 matches with some evaluation failures => surface the first error.
//   - 0 matches with no failures => the target is not managed by any eligible
//     Application: deny.
//
// The (result, error) contract is the fail-closed hinge: any non-nil error (or a
// nil result) MUST be treated by callers as "do not act".
func (m *Mapper) resolveGoverningApplication(ctx context.Context, getter applicationGetter, group grp.InstanceGroup, instance chaosmonkey.Instance) (*ResolvedApplication, error) {
	candidates := m.EligibleApplications()
	if len(candidates) == 0 {
		return nil, errors.New("argocd: no eligible Applications configured")
	}

	var (
		matches  []*ResolvedApplication
		firstErr error
		errCount int
	)
	for _, name := range candidates {
		app, err := getter.GetApplication(ctx, name)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			errCount++
			continue
		}
		// Defensive: the server MUST return the Application we asked for. A
		// name mismatch (rename, redirect, or misrouting) is treated as an
		// evaluation failure for this candidate so a wrong Application can never
		// be used as the governing target.
		if app == nil || strings.TrimSpace(app.Metadata.Name) != name {
			if firstErr == nil {
				firstErr = errors.Errorf("argocd: lookup for %q returned a different Application", name)
			}
			errCount++
			continue
		}
		if resource, ok := m.ManagedResourceFor(app, group, instance); ok {
			matches = append(matches, &ResolvedApplication{app: app, resource: resource, name: name})
		}
	}

	switch {
	case len(matches) > 1:
		return nil, errors.Errorf("argocd: target matched %d eligible Applications %v (ambiguous ownership); refusing to act",
			len(matches), matchNames(matches))
	case len(matches) == 1 && errCount > 0:
		return nil, errors.Wrapf(firstErr,
			"argocd: target matched Application %q but %d eligible Application(s) could not be evaluated (cannot prove unique ownership)",
			matches[0].name, errCount)
	case len(matches) == 1:
		return matches[0], nil
	case errCount > 0:
		return nil, errors.Wrap(firstErr, "argocd: could not resolve governing Application")
	default:
		return nil, errors.New("argocd: no eligible Application manages the target")
	}
}

// matchNames extracts the Application names from resolved matches for a
// human-readable ambiguity error. Names are configuration values, not secrets.
func matchNames(matches []*ResolvedApplication) []string {
	names := make([]string, len(matches))
	for i, mm := range matches {
		names[i] = mm.name
	}
	return names
}
