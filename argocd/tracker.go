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
	"encoding/json"
	"log"
	"time"

	"github.com/Netflix/chaosmonkey/v2"
	"github.com/Netflix/chaosmonkey/v2/config"
)

// annotationKey is the Application annotation Chaos Monkey writes to record the
// most recent termination it performed against the Application's workloads. It
// is visible through the Argo CD API and the Application's metadata/manifest
// view in the UI, surfacing the chaos action in the shared GitOps timeline.
//
// Optional Argo CD Notifications: operators who run the (optional) Argo CD
// Notifications controller can additionally subscribe to events via annotations
// of the form notifications.argoproj.io/subscribe.<trigger>.<service> (AAP
// 0.2.2). That controller is not a prerequisite. This tracker writes ONLY the
// custom chaosmonkey.netflix.com/last-termination annotation below and does not
// create or manage any notifications.argoproj.io subscription annotations.
const annotationKey = "chaosmonkey.netflix.com/last-termination"

// terminationAnnotation is the compact JSON payload stored in annotationKey.
// The chaosmonkey.Termination type carries no termination identifier (only
// Instance, Time, and Leashed), so the instance id doubles as the human-readable
// subject of the record; no termination-id field is fabricated.
type terminationAnnotation struct {
	Instance string `json:"instance"`
	Time     string `json:"time"`
	Leashed  bool   `json:"leashed"`
}

// argoTracker is a best-effort chaosmonkey.Tracker that records terminations as
// an annotation on the governing Argo CD Application. It is non-blocking: every
// failure is logged and swallowed so a write-back error can never fail a
// termination. (Added for the Argo CD integration.)
type argoTracker struct {
	client  *Client
	mapper  *Mapper
	timeout time.Duration
}

// NewTracker builds the Argo CD write-back tracker. It is intentionally lenient:
// any configuration/client-construction problem is logged and degraded to a
// no-op tracker (nil client) rather than returned as an error, because the
// tracker factory (tracker.getTrackers) treats a construction error as fatal to
// the entire terminate run. The returned tracker still satisfies
// chaosmonkey.Tracker and its Track always succeeds.
//
// The signature matches the tracker factory's `case "argocd"` extension point
// exactly: func(cfg *config.Monkey) (chaosmonkey.Tracker, error). The error
// return is present only to satisfy that signature and is always nil.
func NewTracker(cfg *config.Monkey) (chaosmonkey.Tracker, error) {
	c, err := configFromMonkey(cfg)
	if err != nil {
		// A malformed [argocd] section must not abort the terminate run.
		log.Printf("argocd tracker: disabled, could not read config: %v", err)
		return argoTracker{}, nil
	}

	t := argoTracker{
		mapper:  NewMapper(c.Applications),
		timeout: c.Timeout,
	}

	// No endpoint means the feature is disabled/unconfigured: the tracker is
	// inert (nil client) and Track is a no-op.
	if c.Endpoint == "" {
		log.Printf("argocd tracker: no endpoint configured; write-back disabled")
		return t, nil
	}

	client, err := NewClient(c)
	if err != nil {
		log.Printf("argocd tracker: could not build client; write-back disabled: %v", err)
		return t, nil
	}
	t.client = client
	return t, nil
}

// Track records the termination as a best-effort annotation on the governing
// Argo CD Application. It ALWAYS returns nil: every failure mode (no client,
// missing/typed-nil instance, unresolved Application, marshal error, HTTP error)
// is logged and swallowed so that a write-back problem can never block or fail a
// termination.
//
// This is the mirror image of the precheck's fail-closed posture: the gate
// denies on uncertainty, whereas the tracker never surfaces an error because
// term.doTerminate treats a tracker error as fatal to the termination that has
// already been recorded.
func (t argoTracker) Track(trm chaosmonkey.Termination) error {
	// Write-back not configured (feature disabled, or client construction
	// degraded to a no-op): nothing to do.
	if t.client == nil {
		return nil
	}

	// Guard against a nil or typed-nil instance so a best-effort write-back can
	// never panic the scheduler. isNilInstance is the same-package helper that
	// mapper.go uses for the identical purpose.
	if isNilInstance(trm.Instance) {
		log.Printf("argocd tracker: termination carries no usable instance; skipping write-back")
		return nil
	}

	// Bound the entire write-back (target-resolution GETs plus the annotation
	// PATCH) by the configured timeout so a slow or hung Argo CD API can never
	// stall the terminate run.
	ctx, cancel := context.WithTimeout(context.Background(), t.timeout)
	defer cancel()

	// Resolve the Argo CD Application that authoritatively manages the
	// terminated instance. The delivered Mapper is client-free by design and
	// deliberately never infers ownership from name coincidence, so resolution
	// correlates the instance against each eligible Application's authoritative
	// status.resources[] inventory (see resolveApplication).
	name, ok := t.resolveApplication(ctx, trm.Instance)
	if !ok {
		log.Printf("argocd tracker: could not resolve Application for instance %s; skipping write-back", trm.Instance.ID())
		return nil
	}

	payload, err := json.Marshal(terminationAnnotation{
		Instance: trm.Instance.ID(),
		Time:     trm.Time.Format(time.RFC3339),
		Leashed:  trm.Leashed,
	})
	if err != nil {
		log.Printf("argocd tracker: could not marshal annotation for %s: %v", name, err)
		return nil
	}

	if err := t.client.PatchApplicationAnnotation(ctx, name, annotationKey, string(payload)); err != nil {
		log.Printf("argocd tracker: best-effort annotation write to %s failed: %v", name, err)
		return nil
	}
	return nil
}

// resolveApplication finds the eligible Argo CD Application that authoritatively
// manages the terminated instance. Because the Mapper does not itself perform
// Argo CD lookups, the tracker queries each configured eligible Application by
// name and returns the first whose reported status.resources[] inventory maps to
// the instance (Mapper.ManagedResourceFor). The tracker has no InstanceGroup in
// scope, so nil is passed (instance-only resolution); ManagedResourceFor and its
// identifier helper both accept a nil group.
//
// It returns ok=false when no governing Application can be resolved — none
// eligible, every lookup failed, or none manages the instance — in which case
// the caller skips the write-back. Per-Application lookup errors are logged and
// skipped rather than aborting resolution, preserving the best-effort contract.
func (t argoTracker) resolveApplication(ctx context.Context, instance chaosmonkey.Instance) (string, bool) {
	for _, name := range t.mapper.EligibleApplications() {
		app, err := t.client.GetApplication(ctx, name)
		if err != nil {
			log.Printf("argocd tracker: could not fetch Application %q while resolving write-back target: %v", name, err)
			continue
		}
		if _, ok := t.mapper.ManagedResourceFor(app, nil, instance); ok {
			return app.Metadata.Name, true
		}
	}
	return "", false
}
