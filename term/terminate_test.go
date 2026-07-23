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

package term

import (
	"errors"
	"testing"
	"time"

	"github.com/Netflix/chaosmonkey/v2"
	"github.com/Netflix/chaosmonkey/v2/clock"
	"github.com/Netflix/chaosmonkey/v2/config"
	"github.com/Netflix/chaosmonkey/v2/config/param"
	"github.com/Netflix/chaosmonkey/v2/deps"
	"github.com/Netflix/chaosmonkey/v2/grp"
	"github.com/Netflix/chaosmonkey/v2/mock"
)

func mockDeps() deps.Deps {
	monkeyCfg := config.Defaults()
	monkeyCfg.Set(param.Enabled, true)
	monkeyCfg.Set(param.Leashed, false)
	monkeyCfg.Set(param.Accounts, []string{"prod"})
	recorder := mock.Checker{Error: nil}
	confGetter := mock.DefaultConfigGetter()
	cl := clock.New()
	dep := mock.Dep()
	ttor := mock.Terminator{}
	ou := mock.Outage{}
	env := mock.Env{IsInTest: false}
	return deps.Deps{MonkeyCfg: monkeyCfg, Checker: recorder, ConfGetter: confGetter, Cl: cl, Dep: dep, T: &ttor, Ou: ou, Env: env}
}

// TestTerminateKills ensure the terminator actually gets invoked
func TestTerminateKills(t *testing.T) {

	deps := mockDeps()
	err := Terminate(deps, "foo", "prod", "us-east-1", "", "foo-prod")

	if err != nil {
		t.Fatal(err)
	}

	ttor := deps.T.(*mock.Terminator)
	ins := ttor.Instance

	if got, want := ttor.Ncalls, 1; got != want {
		t.Fatalf("Expected terminator to be called once, got ttor.Ncalls=%d", ttor.Ncalls)
	}

	if got, want := ins.AppName(), "foo"; got != want {
		t.Errorf("Expected ins.AppName()=%s. want %s", got, want)
	}

	if got, want := ins.AccountName(), "prod"; got != want {
		t.Errorf("Expected ins.AccountName()=%s. want %s", got, want)
	}

	if got, want := ins.RegionName(), "us-east-1"; got != want {
		t.Errorf("Expected ins.RegionName()=%s. want %s", got, want)
	}

	if got, want := ins.ClusterName(), "foo-prod"; got != want {
		t.Errorf("Expected ins.ClusterName()=%s. want %s", got, want)
	}
}

// TestTerminateOnlyKillsInProd ensures we don't kill in non-prod accounts
// This is temporary until we have full support for multiple accounts
func TestTerminateOnlyKillsInProd(t *testing.T) {
	deps := mockDeps()

	err := Terminate(deps, "quux", "test", "us-east-1", "", "quux-test")

	if err != nil {
		t.Fatal(err)
	}

	ttor := deps.T.(*mock.Terminator)
	if got, want := ttor.Ncalls, 0; got != want {
		t.Errorf("Expected terminator to not be called, got ttor.Ncalls=%d", ttor.Ncalls)
	}

}

func TestTerminateDoesntKillIfRecorderFails(t *testing.T) {
	deps := mockDeps()
	deps.Checker = mock.Checker{Error: chaosmonkey.ErrViolatesMinTime{InstanceID: "i-8703ada6", KilledAt: time.Now().Add(-1 * time.Hour)}}

	err := Terminate(deps, "foo", "prod", "us-east-1", "", "foo-prod")
	if err == nil {
		t.Fatal("Expected Terminate to fail, it succeeded")
	}

	ttor := deps.T.(*mock.Terminator)
	if got, want := ttor.Ncalls, 0; got != want {
		t.Errorf("Expected terminator to not be called, got ttor.Ncalls=%d", ttor.Ncalls)
	}
}

// TestTerminateDoesntKillInLeashedMode ensure terminator does not get invoked
// if leashed is enabled
func TestTerminateDoesntKillInLeashedMode(t *testing.T) {

	deps := mockDeps()
	cfg := config.Defaults()
	// Setting leashed explicitly for code clarity, default is leashed so
	// this isn't strictly neededj
	cfg.Set(param.Leashed, true)

	deps.MonkeyCfg = cfg

	err := Terminate(deps, "foo", "prod", "us-east-1", "", "foo-prod")

	if err != nil {
		t.Fatal(err)
	}

	ttor := deps.T.(*mock.Terminator)
	if got, want := ttor.Ncalls, 0; got != want {
		t.Errorf("Expected terminator to not be called, got ttor.Ncalls=%d", ttor.Ncalls)
	}

}

// TestNeverTerminateInTestEnv checks that unleasshed terms are not allowed in
// test
func TestNeverTerminateUnleashedInTestEnv(t *testing.T) {

	deps := mockDeps()
	deps.Env = mock.Env{IsInTest: true}

	err := Terminate(deps, "foo", "prod", "us-east-1", "", "foo-prod")

	if _, ok := err.(UnleashedInTestEnv); !ok {
		t.Fatalf("Expected Terminate to return an error when running unleashed in test mode")
	}

	ttor := deps.T.(*mock.Terminator)
	if got, want := ttor.Ncalls, 0; got != want {
		t.Errorf("Expected terminator to be called once, got ttor.Ncalls=%d", ttor.Ncalls)
	}

}

func TestDoesNotTerminateIfTrackerFails(t *testing.T) {
	deps := mockDeps()

	// We pass two trackers, the first one succeeds, the second returns an error
	deps.Trackers = []chaosmonkey.Tracker{
		mock.Tracker{},
		mock.Tracker{Error: errors.New("something went wrong")}}

	err := Terminate(deps, "foo", "prod", "us-east-1", "", "foo-prod")
	if err == nil {
		t.Fatal("Tracker failed but Terminate did not return an error")
	}

	ttor := deps.T.(*mock.Terminator)
	if got, want := ttor.Ncalls, 0; got != want {
		t.Errorf("Expected terminator to not be called, got ttor.Ncalls=%d", ttor.Ncalls)
	}

}

func TestDoesNotTerminateIfAppIsDisabled(t *testing.T) {
	deps := mockDeps()

	// Disable app
	deps.ConfGetter = mock.NewConfigGetter(chaosmonkey.AppConfig{
		Enabled:                        false,
		RegionsAreIndependent:          true,
		MeanTimeBetweenKillsInWorkDays: 5,
		MinTimeBetweenKillsInWorkDays:  1,
		Grouping:                       chaosmonkey.Cluster,
		Exceptions:                     nil,
	})

	err := Terminate(deps, "foo", "prod", "us-east-1", "", "foo-prod")
	if err != nil {
		t.Fatal(err)
	}

	ttor := deps.T.(*mock.Terminator)
	if got, want := ttor.Ncalls, 0; got != want {
		t.Errorf("Expected terminator to not be called, got ttor.Ncalls=%d", ttor.Ncalls)
	}
}

// stubPrecheck is a test-only chaosmonkey.Precheck whose decision is fixed. It
// records how many times Allow was invoked so the orchestration test can prove
// the gate is consulted exactly once per termination attempt. (Added for the
// Argo CD integration per code review M-05.)
type stubPrecheck struct {
	allowed bool
	reason  string
	target  string
	err     error
	calls   int
}

// Allow satisfies chaosmonkey.Precheck. It returns the fixed target so the
// orchestration test can prove the gate-resolved target is carried onto the
// Termination (Argo CD integration, QA findings F3/F4/F5).
func (p *stubPrecheck) Allow(group grp.InstanceGroup, instance chaosmonkey.Instance) (bool, string, string, error) {
	p.calls++
	return p.allowed, p.reason, p.target, p.err
}

// Compile-time proof the stub satisfies the additive gate contract.
var _ chaosmonkey.Precheck = (*stubPrecheck)(nil)

// recordingTracker is a test-only chaosmonkey.Tracker that captures the last
// Termination it received, so a test can prove the gate-resolved target is
// carried onto Termination.Target and handed to trackers (Argo CD integration,
// QA findings F3/F4/F5).
type recordingTracker struct {
	last  chaosmonkey.Termination
	calls int
}

// Track satisfies chaosmonkey.Tracker; it records the termination and succeeds.
func (t *recordingTracker) Track(trm chaosmonkey.Termination) error {
	t.calls++
	t.last = trm
	return nil
}

// Compile-time proof the recording tracker satisfies the contract.
var _ chaosmonkey.Tracker = (*recordingTracker)(nil)

// TestTerminatePrecheckGate is the orchestration proof (code review M-05) that
// term.doTerminate wires the additive Argo CD pre-flight gate correctly: an
// allow proceeds to the kill, a deny skips the termination WITHOUT an error, and
// a gate error fails closed by propagating the error and NOT killing. In every
// case the gate is consulted exactly once, after the target instance is picked.
func TestTerminatePrecheckGate(t *testing.T) {
	cases := []struct {
		name       string
		precheck   *stubPrecheck
		wantErr    bool
		wantNcalls int
		wantTarget string
	}{
		{"allow proceeds to the kill", &stubPrecheck{allowed: true, target: "argocd-app-x"}, false, 1, "argocd-app-x"},
		{"deny skips termination without error", &stubPrecheck{allowed: false, reason: "argocd: application not synced"}, false, 0, ""},
		{"gate error fails closed and propagates", &stubPrecheck{allowed: false, err: errors.New("argocd unreachable")}, true, 0, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := mockDeps()
			d.Precheck = tc.precheck
			// Attach a recording tracker so we can assert the gate-resolved
			// target is carried onto the Termination and handed to trackers.
			rec := &recordingTracker{}
			d.Trackers = []chaosmonkey.Tracker{rec}

			err := Terminate(d, "foo", "prod", "us-east-1", "", "foo-prod")
			if tc.wantErr && err == nil {
				t.Fatal("expected Terminate to return an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected Terminate to succeed, got %v", err)
			}

			ttor := d.T.(*mock.Terminator)
			if ttor.Ncalls != tc.wantNcalls {
				t.Errorf("terminator Ncalls = %d, want %d", ttor.Ncalls, tc.wantNcalls)
			}
			if tc.precheck.calls != 1 {
				t.Errorf("precheck consulted %d times, want exactly 1", tc.precheck.calls)
			}

			// When the gate allowed and the termination proceeded, the
			// gate-resolved target must have been carried onto Termination.Target
			// and handed to the tracker (the term.go wiring for F3/F4/F5).
			if tc.wantNcalls > 0 {
				if rec.calls != 1 {
					t.Errorf("tracker consulted %d times, want exactly 1", rec.calls)
				}
				if rec.last.Target != tc.wantTarget {
					t.Errorf("tracker received Termination.Target=%q, want %q", rec.last.Target, tc.wantTarget)
				}
			}
		})
	}
}

// TestTerminateNilPrecheckAllowsAll proves a nil Precheck provider is treated as
// allow-all, so existing behavior is preserved byte-for-byte when the Argo CD
// feature is unconfigured (code review m-01/M-05). mockDeps leaves Precheck nil.
func TestTerminateNilPrecheckAllowsAll(t *testing.T) {
	d := mockDeps()
	if d.Precheck != nil {
		t.Fatal("precondition failed: mockDeps should leave Precheck nil")
	}

	if err := Terminate(d, "foo", "prod", "us-east-1", "", "foo-prod"); err != nil {
		t.Fatal(err)
	}

	ttor := d.T.(*mock.Terminator)
	if got, want := ttor.Ncalls, 1; got != want {
		t.Errorf("terminator Ncalls = %d, want %d (a nil Precheck must allow-all)", got, want)
	}
}
