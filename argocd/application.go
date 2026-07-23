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

// Application is the minimal subset of an Argo CD argoproj.io/v1alpha1
// Application resource that the Chaos Monkey integration needs. It is decoded
// from GET /api/v1/applications/{name}. Only the fields consumed by the
// sync/health gate and the annotation write-back are modeled.
type Application struct {
	Metadata ObjectMeta        `json:"metadata"`
	Status   ApplicationStatus `json:"status"`
}

// ObjectMeta is the subset of Kubernetes object metadata the integration reads.
type ObjectMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Annotations map[string]string `json:"annotations"`
}

// ApplicationStatus is the subset of an Argo CD Application's status that the
// integration inspects: aggregate sync state, aggregate health state, and the
// list of managed resources.
type ApplicationStatus struct {
	Sync      SyncStatus       `json:"sync"`
	Health    HealthStatus     `json:"health"`
	Resources []ResourceStatus `json:"resources"`
}

// SyncStatus holds the Application-level sync state (Synced / OutOfSync).
type SyncStatus struct {
	Status string `json:"status"`
}

// HealthStatus holds a health state string (Healthy / Progressing / Degraded /
// Suspended / Missing / Unknown). It is used both at the Application level and
// per managed resource.
type HealthStatus struct {
	Status string `json:"status"`
}

// ResourceStatus describes a single resource managed by the Application, as
// reported in status.resources[]. Health is a pointer because Argo CD may omit
// it for resources that do not report health.
//
// Hook and RequiresPruning mirror the official argoproj.io ResourceStatus
// fields. A resource that is a sync Hook (e.g. a one-shot Job) or that is only
// pending pruning is NOT a stable, chaos-eligible managed workload, so both are
// modeled and excluded from target ownership (added per code review C-02).
type ResourceStatus struct {
	Group           string        `json:"group"`
	Version         string        `json:"version"`
	Kind            string        `json:"kind"`
	Namespace       string        `json:"namespace"`
	Name            string        `json:"name"`
	Status          string        `json:"status"`
	Health          *HealthStatus `json:"health,omitempty"`
	Hook            bool          `json:"hook,omitempty"`
	RequiresPruning bool          `json:"requiresPruning,omitempty"`
}

// Sync status values reported by Argo CD (status.sync.status).
const (
	SyncStatusSynced    = "Synced"
	SyncStatusOutOfSync = "OutOfSync"
)

// Health status values reported by Argo CD (status.health.status and per-resource health).
const (
	HealthStatusHealthy     = "Healthy"
	HealthStatusProgressing = "Progressing"
	HealthStatusDegraded    = "Degraded"
	HealthStatusSuspended   = "Suspended"
	HealthStatusMissing     = "Missing"
	HealthStatusUnknown     = "Unknown"
)

// IsSynced reports whether the Application's aggregate sync status is exactly
// Synced. A nil receiver returns false so the sync/health gate fails closed.
// (Nil-safety added for the Argo CD integration per code review M-04.)
func (a *Application) IsSynced() bool {
	return a != nil && a.Status.Sync.Status == SyncStatusSynced
}

// IsHealthy reports whether the Application's aggregate health status is exactly
// Healthy. A nil receiver returns false so the gate fails closed. Exact equality
// also rejects unknown/future health strings.
// (Nil-safety added for the Argo CD integration per code review M-04.)
func (a *Application) IsHealthy() bool {
	return a != nil && a.Status.Health.Status == HealthStatusHealthy
}

// isWorkloadGVK reports whether (group, kind) identifies a Kubernetes workload
// kind that Chaos Monkey treats as chaos-eligible, in its expected API group.
// Binding each kind to its API group prevents a foreign resource that merely
// reuses a workload Kind string (in a different group) from being accepted.
// (Added for the Argo CD integration per code review M-01.)
func isWorkloadGVK(group, kind string) bool {
	switch kind {
	case "Pod":
		return group == "" // core API group
	case "Deployment", "ReplicaSet", "StatefulSet", "DaemonSet":
		return group == "apps"
	case "Rollout":
		return group == "argoproj.io" // Argo Rollouts CRD
	default:
		return false
	}
}

// IsWorkload reports whether the resource is a supported workload kind in its
// expected API group (Pod in the core group; Deployment/ReplicaSet/StatefulSet/
// DaemonSet in "apps"; Rollout in "argoproj.io"). Non-workload kinds such as
// Service or ConfigMap, and workload Kinds carried in an unexpected group,
// return false. (Added for the Argo CD integration per code review M-01.)
func (r ResourceStatus) IsWorkload() bool {
	return isWorkloadGVK(r.Group, r.Kind)
}

// IsLive reports whether Argo CD indicates the resource currently exists as a
// live object. It uses a strict ALLOW-LIST of known-live health states
// (Healthy / Progressing / Degraded / Suspended) so that an absent health, a
// Missing/Unknown health, or any unrecognized/future health string all fail
// closed as "not proven live". Because ResourceStatus.Status is a
// *synchronization* state (Synced/OutOfSync) rather than proof of a live object,
// only the health field is consulted here.
// (Changed from a reject-list to a fail-closed allow-list per code review C-02;
// originally added per code review M-01.)
func (r ResourceStatus) IsLive() bool {
	if r.Health == nil {
		return false
	}
	switch r.Health.Status {
	case HealthStatusHealthy, HealthStatusProgressing, HealthStatusDegraded, HealthStatusSuspended:
		return true
	default:
		// Missing, Unknown, empty, or any unrecognized future value: fail closed.
		return false
	}
}

// IsLiveWorkload reports whether the resource is a stable, chaos-eligible
// managed workload: a supported workload kind (IsWorkload) that is currently
// live (IsLive) and is neither a sync Hook nor a resource pending pruning. Hook
// resources (e.g. one-shot Jobs) and prune-pending resources are transient
// GitOps artifacts rather than steady-state workloads, so they are excluded from
// target ownership. This is the precise per-resource safety predicate the target
// gate relies on.
// (Hook/RequiresPruning exclusion added per code review C-02; originally added
// per code review M-01.)
func (r ResourceStatus) IsLiveWorkload() bool {
	if r.Hook || r.RequiresPruning {
		return false
	}
	return r.IsWorkload() && r.IsLive()
}

// LiveWorkloads returns the managed resources that are live workloads
// (IsLiveWorkload), preserving their reported order. A nil Application yields
// nil. (Added for the Argo CD integration per code review M-01/M-04.)
func (a *Application) LiveWorkloads() []ResourceStatus {
	if a == nil {
		return nil
	}
	var out []ResourceStatus
	for _, r := range a.Status.Resources {
		if r.IsLiveWorkload() {
			out = append(out, r)
		}
	}
	return out
}

// HasLiveResources reports whether the Application manages at least one live
// workload resource. Unlike a bare non-empty status.resources[] check, this
// requires a supported workload kind that Argo CD reports as live, so an
// Application managing only Services/ConfigMaps, or only Missing resources, is
// correctly treated as ineligible (fail-closed). A nil receiver returns false.
// (Updated for the Argo CD integration per code review M-01/M-04.)
func (a *Application) HasLiveResources() bool {
	return len(a.LiveWorkloads()) > 0
}
