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
	"fmt"
	"time"

	"github.com/Netflix/chaosmonkey/v2"
	"github.com/Netflix/chaosmonkey/v2/config"
	"github.com/Netflix/chaosmonkey/v2/deps"
	"github.com/Netflix/chaosmonkey/v2/grp"
	"github.com/pkg/errors"
)

// init registers the Argo CD pre-flight gate with the dependency-injection
// layer, following the same plugin pattern as the outage and constrainer
// providers. This is part of the Argo CD integration (opt-in via [argocd]).
func init() {
	deps.GetPrecheck = GetPrecheck
}

// allowAllPrecheck is the inert provider returned when the Argo CD integration
// is disabled or unconfigured. It permits every termination, preserving
// existing behavior byte-for-byte (mirroring outage.NullOutage).
type allowAllPrecheck struct{}

// Allow always permits the termination. Because it never errors, a disabled or
// unconfigured Argo CD integration is completely inert. It resolves no target
// (empty string), so a downstream tracker has nothing to write back to.
func (allowAllPrecheck) Allow(group grp.InstanceGroup, instance chaosmonkey.Instance) (bool, string, string, error) {
	return true, "", "", nil
}

// argoPrecheck is the Argo CD-backed pre-flight gate. It resolves the governing
// Application for a target and permits the termination only when that
// Application is Synced and Healthy. It is fail-closed: any uncertainty results
// in a deny (skip), never a crash.
type argoPrecheck struct {
	client  *Client
	mapper  *Mapper
	timeout time.Duration
}

// Compile-time assertions that both providers satisfy chaosmonkey.Precheck.
var (
	_ chaosmonkey.Precheck = (*argoPrecheck)(nil)
	_ chaosmonkey.Precheck = allowAllPrecheck{}
)

// GetPrecheck builds the pre-flight gate provider from configuration. When the
// Argo CD integration is disabled it returns an allow-all provider so the
// feature is completely inert. When enabled it constructs the REST client and
// mapper and returns the Argo CD-backed gate. It returns an error only for
// genuine misconfiguration (enabled but missing endpoint / bad TLS material);
// the gate itself is fail-closed at call time.
func GetPrecheck(cfg *config.Monkey) (chaosmonkey.Precheck, error) {
	// Guard against a nil configuration so the DI factory never panics before it
	// can dereference cfg (cfg.ArgoCDEnabled below). A nil *config.Monkey is a
	// wiring/programming error at the injection layer rather than a user setting,
	// so surface it explicitly instead of silently going inert. (Nil-safety
	// hardening added per code review Q-09.)
	if cfg == nil {
		return nil, errors.New("argocd: GetPrecheck received a nil configuration")
	}

	// Disabled short-circuit: return the inert allow-all provider so existing
	// behavior is preserved byte-for-byte when [argocd] is absent or disabled.
	if !cfg.ArgoCDEnabled() {
		return allowAllPrecheck{}, nil
	}

	c, err := configFromMonkey(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "argocd: could not build precheck config")
	}
	if c.Endpoint == "" {
		return nil, errors.New("argocd: enabled but no endpoint configured")
	}

	client, err := NewClient(c)
	if err != nil {
		return nil, errors.Wrap(err, "argocd: could not build precheck client")
	}

	return &argoPrecheck{
		client:  client,
		mapper:  NewMapper(c.Applications),
		timeout: c.Timeout,
	}, nil
}

// Allow implements chaosmonkey.Precheck. It permits a termination only when the
// governing Argo CD Application is Synced and Healthy. Every uncertain or
// negative condition results in a graceful deny (skip) with a descriptive
// reason and a nil error, so the scheduler is never crashed and unstable
// workloads are never disrupted.
//
// The governing Application is resolved by the shared, fail-closed resolver
// (Mapper.resolveGoverningApplication), which correlates the target against the
// authoritative status.resources[] inventory of EVERY operator-configured
// eligible Application and proves GLOBAL uniqueness: it denies when more than
// one eligible Application manages the target (ambiguous ownership), when a
// unique match cannot be proven because some eligible Application could not be
// evaluated, and when a lookup fails or no eligible Application manages the
// target.
//
// On allow, the resolved Application name is returned as the target so the
// caller can carry it (via Termination.Target) into the write-back tracker.
// The tracker annotates exactly this gate-authorized identity rather than
// re-resolving ownership itself, which guarantees the gate and the annotation
// can never disagree about which Application a target belongs to — even if
// ownership changes between the gate and the write-back (code review C-03/C-04;
// QA findings F3/F4/F5).
func (p *argoPrecheck) Allow(group grp.InstanceGroup, instance chaosmonkey.Instance) (bool, string, string, error) {
	// Defensive nil guards: a malformed provider must fail closed (skip) rather
	// than panic and crash the scheduler. In normal operation GetPrecheck always
	// populates these fields, but a nil receiver, client, or mapper would
	// otherwise dereference-panic below (p.timeout / p.mapper / p.client). The
	// short-circuit order makes the nil-receiver check safe. (Nil-safety
	// hardening added per code review Q-09.)
	if p == nil || p.client == nil || p.mapper == nil {
		return false, "argocd: precheck provider is not fully initialized", "", nil
	}

	// A single deadline bounds the whole gate evaluation so a slow or
	// unreachable Argo CD API can never stall the scheduler indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	// Resolve the governing Application with proven global uniqueness. Any
	// ambiguity, unprovable uniqueness, lookup error, or no-match is returned as
	// a non-nil error, which we convert into a graceful deny (skip) with the
	// resolver's descriptive reason and a nil error (fail-closed).
	resolved, err := p.mapper.resolveGoverningApplication(ctx, p.client, group, instance)
	if err != nil {
		return false, err.Error(), "", nil
	}

	governing := resolved.Application()
	govName := resolved.Name()

	// Defense-in-depth: a successful resolution already implies at least one
	// live workload, but re-check so any future divergence still fails closed.
	if !governing.HasLiveResources() {
		return false, fmt.Sprintf("argocd: application %q manages no live resources", govName), "", nil
	}

	// The only allow path: the governing Application is both Synced and Healthy.
	// Return govName as the resolved target so the write-back tracker annotates
	// exactly this Application (carried via Termination.Target).
	if governing.IsSynced() && governing.IsHealthy() {
		return true, "", govName, nil
	}

	// Fail-closed: the Application is not eligible for chaos right now. Name both
	// the sync and health status so Progressing/OutOfSync/Degraded/Suspended/
	// Missing/Unknown are all visible in the scheduler logs.
	//
	// The sync/health status strings come verbatim from the Argo CD API response
	// and are printed with %s, so sanitize them to strip/escape control
	// characters and cap their length. This prevents a hostile or malformed
	// status value from injecting newlines or forged entries into the scheduler
	// log. (govName is the trusted, operator-configured name printed with %q,
	// which already escapes control characters, so it is left as-is.)
	// (Log-injection hardening added per code review Q-15.)
	return false, fmt.Sprintf(
		"argocd: application %q not eligible (sync=%s, health=%s)",
		govName,
		sanitizeForLog(governing.Status.Sync.Status),
		sanitizeForLog(governing.Status.Health.Status),
	), "", nil
}
