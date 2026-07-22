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
type ResourceStatus struct {
	Group     string        `json:"group"`
	Version   string        `json:"version"`
	Kind      string        `json:"kind"`
	Namespace string        `json:"namespace"`
	Name      string        `json:"name"`
	Status    string        `json:"status"`
	Health    *HealthStatus `json:"health,omitempty"`
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

// IsSynced reports whether the Application's aggregate sync status is Synced.
func (a *Application) IsSynced() bool {
	return a.Status.Sync.Status == SyncStatusSynced
}

// IsHealthy reports whether the Application's aggregate health status is Healthy.
func (a *Application) IsHealthy() bool {
	return a.Status.Health.Status == HealthStatusHealthy
}

// HasLiveResources reports whether the Application manages at least one
// resource (status.resources[] is non-empty). The sync/health gate treats an
// Application that manages zero resources as ineligible (fail-closed).
func (a *Application) HasLiveResources() bool {
	return len(a.Status.Resources) > 0
}
