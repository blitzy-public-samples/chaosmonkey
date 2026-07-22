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
// unconfigured Argo CD integration is completely inert.
func (allowAllPrecheck) Allow(group grp.InstanceGroup, instance chaosmonkey.Instance) (bool, string, error) {
	return true, "", nil
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
// The governing Application is resolved by correlating the target against the
// authoritative status.resources[] inventory of each operator-configured
// eligible Application (via the mapper), rather than by name coincidence. The
// first eligible Application that manages the target is treated as governing.
func (p *argoPrecheck) Allow(group grp.InstanceGroup, instance chaosmonkey.Instance) (bool, string, error) {
	// The operator allow-list is the sole source of eligibility. An empty list
	// authorizes nothing (fail-closed).
	candidates := p.mapper.EligibleApplications()
	if len(candidates) == 0 {
		return false, "argocd: no eligible Applications configured", nil
	}

	// A single deadline bounds the whole gate evaluation so a slow or
	// unreachable Argo CD API can never stall the scheduler indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	// Resolve the governing Application: fetch each eligible Application and
	// accept the first one whose authoritative resource inventory manages the
	// selected target. Fetch errors are remembered but non-fatal to the loop,
	// so one unreachable/renamed Application cannot mask another that governs
	// the target; if none resolves we fail closed below.
	var (
		governing *Application
		govName   string
		lastErr   error
	)
	for _, name := range candidates {
		app, err := p.client.GetApplication(ctx, name)
		if err != nil {
			lastErr = err
			continue
		}
		if _, ok := p.mapper.ManagedResourceFor(app, group, instance); ok {
			governing = app
			govName = name
			break
		}
	}

	// Fail-closed branch 1: the target could not be tied to any eligible,
	// live-workload-managing Application. Surface the last API error, if any,
	// so an unreachable API or missing/renamed Application is visible in logs.
	if governing == nil {
		if lastErr != nil {
			return false, fmt.Sprintf("argocd: could not resolve governing Application: %v", lastErr), nil
		}
		return false, "argocd: could not resolve governing Application", nil
	}

	// Fail-closed branch 2: the Application manages no live workload resources.
	// (Defense-in-depth: a successful ManagedResourceFor already implies at
	// least one live workload, but this guards against any future divergence.)
	if !governing.HasLiveResources() {
		return false, fmt.Sprintf("argocd: application %q manages no live resources", govName), nil
	}

	// The only allow path: the governing Application is both Synced and Healthy.
	if governing.IsSynced() && governing.IsHealthy() {
		return true, "", nil
	}

	// Fail-closed branch 3: the Application is not eligible for chaos right now.
	// Name both the sync and health status so Progressing/OutOfSync/Degraded/
	// Suspended/Missing/Unknown are all visible in the scheduler logs.
	return false, fmt.Sprintf(
		"argocd: application %q not eligible (sync=%s, health=%s)",
		govName, governing.Status.Sync.Status, governing.Status.Health.Status,
	), nil
}
