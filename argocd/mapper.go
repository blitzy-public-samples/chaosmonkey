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
	"github.com/Netflix/chaosmonkey/v2"
	"github.com/Netflix/chaosmonkey/v2/grp"
)

// Mapper resolves a selected termination target to the name of the Argo CD
// Application that governs it, and extracts the workloads an Application
// manages. Resolution uses the operator-configured list of eligible
// Application names (Config.Applications) together with the target's
// app/cluster identifiers. When the list is empty, resolution falls back to
// the target's own identifiers (the app.kubernetes.io/instance tracking-label
// convention used by Argo CD to associate resources with an Application).
//
// Resolution is intentionally name-based and deterministic: it performs no
// live Kubernetes label lookups, honoring the integration's standard-library
// only constraint. The (name, ok) contract is the fail-closed hinge for the
// sync/health gate — an unresolvable target yields ok=false, which the gate
// converts into a safe skip.
type Mapper struct {
	applications []string // configured eligible Application names (may be empty)
}

// NewMapper builds a Mapper from the configured eligible Application names.
func NewMapper(applications []string) *Mapper {
	return &Mapper{applications: applications}
}

// resolve returns the first Application name that can be determined from the
// ordered candidate identifiers. When an allow-list of Application names is
// configured, only a candidate that exactly matches a configured name is
// accepted (first match wins, in configured order for determinism). When no
// allow-list is configured, the first non-empty candidate is used directly as
// the Application name. Returns ok=false when nothing resolves so the gate can
// fail closed.
//
// The candidate identifiers stand in for the app.kubernetes.io/instance
// tracking label (or the argocd.argoproj.io/tracking-id annotation) that Argo
// CD stamps onto the resources an Application manages. Rather than reading
// those live labels, the fallback path treats the target's cluster/app
// identifier as that instance label, keeping resolution pragmatic.
func (m *Mapper) resolve(candidates []string) (string, bool) {
	if len(m.applications) > 0 {
		// Allow-list configured: iterate the configured names in the OUTER
		// loop so their order deterministically decides ties between
		// candidates. Only an exact match is accepted.
		for _, app := range m.applications {
			for _, cand := range candidates {
				if cand != "" && cand == app {
					return app, true
				}
			}
		}
		return "", false
	}

	// No allow-list: use the first non-empty candidate directly.
	for _, cand := range candidates {
		if cand != "" {
			return cand, true
		}
	}
	return "", false
}

// ApplicationForInstance resolves the governing Argo CD Application name for a
// selected instance, using its cluster then app identifiers as candidates.
func (m *Mapper) ApplicationForInstance(instance chaosmonkey.Instance) (string, bool) {
	candidates := []string{instance.ClusterName(), instance.AppName()}
	return m.resolve(candidates)
}

// ApplicationFor resolves the governing Argo CD Application name for a selected
// target, considering the instance group's app in addition to the instance's
// own cluster/app identifiers. A nil group is tolerated.
func (m *Mapper) ApplicationFor(group grp.InstanceGroup, instance chaosmonkey.Instance) (string, bool) {
	// Candidate priority is deterministic: cluster name first (most specific),
	// then the instance's app name, then the group's app name.
	candidates := []string{instance.ClusterName(), instance.AppName()}
	if group != nil {
		candidates = append(candidates, group.App())
	}
	return m.resolve(candidates)
}

// managedKinds is the set of Kubernetes resource kinds treated as chaos-eligible
// workloads managed by an Argo CD Application.
var managedKinds = map[string]bool{
	"Pod":         true,
	"Deployment":  true,
	"ReplicaSet":  true,
	"StatefulSet": true,
	"DaemonSet":   true,
	"Rollout":     true, // Argo Rollouts CRD
}

// ManagedWorkloads returns the subset of an Application's managed resources that
// are workload kinds (pods/deployments/etc.), read from status.resources[]. A
// nil Application yields nil.
func ManagedWorkloads(app *Application) []ResourceStatus {
	if app == nil {
		return nil
	}
	var out []ResourceStatus
	for _, r := range app.Status.Resources {
		if managedKinds[r.Kind] {
			out = append(out, r)
		}
	}
	return out
}
