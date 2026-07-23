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
	"strings"
	"time"

	"github.com/Netflix/chaosmonkey/v2"
	"github.com/Netflix/chaosmonkey/v2/config"
)

// annotationKey is the Application annotation Chaos Monkey writes to record the
// MOST RECENT termination attempt it made against the Application's workloads. It
// is visible through the Argo CD API and the Application's metadata view in the
// UI, surfacing the latest chaos action alongside the Application in Argo CD. It
// is a single, overwritten "last-termination" breadcrumb — NOT a durable,
// append-only event timeline (see the terminationAnnotation durability note).
//
// ApplicationSet durability (code review M-07): when an Application is generated
// by an ApplicationSet, the ApplicationSet controller reconciles the Application
// and, by default, can strip annotations it does not own. Operators who want this
// breadcrumb to survive reconciliation must add it to the owning ApplicationSet's
// spec.preservedFields.annotations (or a supported global preserved-fields
// policy). This is an operator-side configuration on the ApplicationSet, not
// something this client can set on the managed Application; it is documented in
// docs/plugins/ArgoCD.md.
//
// Optional Argo CD Notifications: operators who run the (optional) Argo CD
// Notifications controller can additionally subscribe to events via annotations
// of the form notifications.argoproj.io/subscribe.<trigger>.<service> (AAP
// 0.2.2). That controller is not a prerequisite. This tracker writes ONLY the
// custom chaosmonkey.netflix.com/last-termination annotation below and does not
// create or manage any notifications.argoproj.io subscription annotations.
const annotationKey = "chaosmonkey.netflix.com/last-termination"

// annotationPhasePreExecution marks the payload as recording a termination
// ATTEMPT written on the pre-kill path (see the terminationAnnotation
// truthfulness note). (Added for the Argo CD integration per code review M-02.)
const annotationPhasePreExecution = "pre-execution"

// terminationAnnotation is the compact JSON payload stored in annotationKey.
//
// Truthfulness (code review M-02): the tracker loop runs BEFORE the killer
// executes — term.doTerminate iterates every tracker and only afterwards calls
// killer.Execute — so this annotation records a termination ATTEMPT that is
// about to be carried out, not a confirmed kill. The Phase field states this
// explicitly ("pre-execution") so a reader of the Argo CD timeline is not misled
// into believing the workload was definitely terminated at the moment of write.
//
// Durability (code review M-02): the annotation holds only the MOST RECENT
// attempt for the Application — a later termination overwrites it. It is a
// lightweight "last-termination" breadcrumb surfaced in the Argo CD UI, not a
// durable, append-only audit trail. Operators needing durable history should
// consume the existing termination store or wire the optional Argo CD
// Notifications controller (AAP 0.2.2).
//
// The chaosmonkey.Termination type carries no termination identifier (its fields
// are Instance, Time, Leashed, and the gate-resolved Target — the governing
// Application name, not a termination id), so the instance id doubles as the
// human-readable subject of the record; no termination-id field is fabricated.
type terminationAnnotation struct {
	Instance string `json:"instance"`
	Time     string `json:"time"`
	Leashed  bool   `json:"leashed"`
	Phase    string `json:"phase"`
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

// maxWriteBackTimeout caps how long a single best-effort annotation write-back
// may run. The tracker loop executes on the pre-kill path (before
// killer.Execute), so this bounds the extra latency a write-back can add to a
// termination independently of the (typically larger) configured argocd.timeout
// that also governs the sync/health gate. (Added for the Argo CD integration per
// code review M-01.)
const maxWriteBackTimeout = 5 * time.Second

// writeBackBudget returns the deadline for one best-effort write-back: the
// smaller of the configured Argo CD timeout and maxWriteBackTimeout. A
// non-positive configured value (which the config layer defaults away) falls
// back to the cap. This keeps the annotation PATCH from stalling the terminate
// run for the full configured timeout. (Added for the Argo CD integration per
// code review M-01.)
func writeBackBudget(configured time.Duration) time.Duration {
	if configured <= 0 || configured > maxWriteBackTimeout {
		return maxWriteBackTimeout
	}
	return configured
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
// Argo CD Application named by Termination.Target (the identity the sync/health
// gate resolved and authorized). It ALWAYS returns nil: every failure mode (no
// client, missing/typed-nil instance, no carried target, ineligible target,
// marshal error, HTTP error) is logged and swallowed so that a write-back
// problem can never block or fail a termination.
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

	// F3/F4/F5 — annotate exactly the Application the sync/health gate authorized.
	// term.doTerminate carries the gate-resolved governing Application name
	// forward on Termination.Target, so the tracker writes back to that identity
	// directly instead of independently re-resolving ownership. This:
	//   - eliminates the redundant resolution GET, so a healthy termination's
	//     Argo CD traffic is [gate GET, checker, PATCH, killer] (F4);
	//   - guarantees a target the gate resolved via its instance group still
	//     receives its write-back, even though Track has no InstanceGroup in
	//     scope and instance-only re-resolution would have failed to match (F3);
	//   - makes an ownership change between the gate and the write-back unable to
	//     redirect the annotation to a DIFFERENT Application, because the target
	//     is fixed at gate time rather than re-resolved here (F5 — TOCTOU).
	// (Reworked for the Argo CD integration per QA findings F3/F4/F5; replaces the
	// previous C-04 instance-only re-resolution.)
	name := strings.TrimSpace(trm.Target)
	if name == "" {
		// No gate-resolved target to write back to: the sync/health gate either
		// did not run or resolved nothing (e.g. the allow-all provider when the
		// feature is disabled). Best-effort: skip without error. trm.Instance.ID()
		// originates outside this package and is printed with %s, so sanitize it
		// (strip/escape control characters, cap length) to prevent log injection
		// (Q-15).
		log.Printf("argocd tracker: termination carries no resolved argocd target; skipping write-back for instance %s", sanitizeForLog(trm.Instance.ID()))
		return nil
	}

	// Defense-in-depth: only ever annotate an operator-configured eligible
	// Application. The carried target already came from the gate resolving against
	// this same eligible allow-list, so this is belt-and-suspenders — it ensures a
	// malformed or unexpected Target can never cause a write to an unlisted
	// Application. name is printed with %q, which escapes control characters.
	if t.mapper != nil && !t.mapper.IsEligible(name) {
		log.Printf("argocd tracker: resolved target %q is not an eligible application; skipping write-back", name)
		return nil
	}

	// Bound the annotation PATCH by the write-back budget — the smaller of
	// argocd.timeout and maxWriteBackTimeout — so a slow or hung Argo CD API on
	// the pre-kill path can never stall the terminate run for the full configured
	// timeout (M-01). No target-resolution GET is issued here; the target was
	// already resolved by the gate and carried forward on the Termination.
	ctx, cancel := context.WithTimeout(context.Background(), writeBackBudget(t.timeout))
	defer cancel()

	payload, err := json.Marshal(terminationAnnotation{
		Instance: trm.Instance.ID(),
		Time:     trm.Time.Format(time.RFC3339),
		Leashed:  trm.Leashed,
		Phase:    annotationPhasePreExecution,
	})
	if err != nil {
		log.Printf("argocd tracker: could not marshal annotation for %s: %v", name, err)
		return nil
	}

	// The PATCH re-confirms the target: PatchApplicationAnnotation rejects a
	// response whose metadata.name differs from the resolved name (a rename or
	// misroute between the resolving GET and the PATCH), and Argo CD enforces the
	// configured project server-side (403). Any such mismatch surfaces here as an
	// error and is swallowed as a skipped write-back (C-04 defense-in-depth).
	if err := t.client.PatchApplicationAnnotation(ctx, name, annotationKey, string(payload)); err != nil {
		log.Printf("argocd tracker: best-effort annotation write to %s failed: %v", name, err)
		return nil
	}
	return nil
}
