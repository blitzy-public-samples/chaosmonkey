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

package tracker

// White-box tests for the tracker factory. They prove the Argo CD integration's
// activation seam (code review M-05): the factory switch selects the Argo CD
// write-back tracker for kind "argocd" (the documented extension point) and
// still returns the unchanged "unsupported tracker" error for any unknown kind,
// so the additive case does not weaken the existing contract. They also prove
// the init() registration side effect that wires deps.GetTrackers.
//
// These tests use only the standard-library testing package, matching the rest
// of the module (testify is not used anywhere in the repository).

import (
	"strings"
	"testing"

	"github.com/Netflix/chaosmonkey/v2/config"
	"github.com/Netflix/chaosmonkey/v2/config/param"
	"github.com/Netflix/chaosmonkey/v2/deps"
)

// TestGetTracker_ArgoCDSelected proves the factory returns a real, non-nil
// tracker for kind "argocd". A disabled default configuration is used, so
// argocd.NewTracker degrades to an inert (nil-client) tracker and returns no
// error — the factory must still hand back a usable chaosmonkey.Tracker.
func TestGetTracker_ArgoCDSelected(t *testing.T) {
	tr, err := getTracker("argocd", config.Defaults())
	if err != nil {
		t.Fatalf("getTracker(\"argocd\") error = %v, want nil", err)
	}
	if tr == nil {
		t.Fatal("getTracker(\"argocd\") returned a nil tracker, want a usable tracker")
	}
}

// TestGetTracker_UnsupportedErrors proves the additive "argocd" case did not
// weaken the default: an unknown tracker kind still yields a nil tracker and the
// unchanged "unsupported tracker" error.
func TestGetTracker_UnsupportedErrors(t *testing.T) {
	tr, err := getTracker("does-not-exist", config.Defaults())
	if err == nil {
		t.Fatal("getTracker(unknown) error = nil, want an unsupported-tracker error")
	}
	if tr != nil {
		t.Errorf("getTracker(unknown) tracker = %v, want nil", tr)
	}
	if !strings.Contains(err.Error(), "unsupported tracker") {
		t.Errorf("getTracker(unknown) error = %q, want it to contain %q", err.Error(), "unsupported tracker")
	}
}

// TestGetTrackers_ArgoCDViaConfig proves the end-to-end selection through the
// public configuration surface: with chaosmonkey.trackers = ["argocd"], the
// factory builds exactly one tracker and returns no error.
func TestGetTrackers_ArgoCDViaConfig(t *testing.T) {
	m := config.Defaults()
	m.Set(param.Trackers, []string{"argocd"})

	trackers, err := getTrackers(m)
	if err != nil {
		t.Fatalf("getTrackers([\"argocd\"]) error = %v, want nil", err)
	}
	if len(trackers) != 1 {
		t.Fatalf("getTrackers([\"argocd\"]) returned %d trackers, want 1", len(trackers))
	}
	if trackers[0] == nil {
		t.Error("getTrackers([\"argocd\"])[0] is nil, want a usable tracker")
	}
}

// TestGetTrackers_UnsupportedPropagatesError proves getTrackers surfaces the
// factory's unsupported-tracker error unchanged when the configured list names
// an unknown tracker.
func TestGetTrackers_UnsupportedPropagatesError(t *testing.T) {
	m := config.Defaults()
	m.Set(param.Trackers, []string{"does-not-exist"})

	if _, err := getTrackers(m); err == nil {
		t.Error("getTrackers([unknown]) error = nil, want an unsupported-tracker error")
	}
}

// TestInit_RegistersDepsGetTrackers proves this package's init() wired
// deps.GetTrackers, the factory the command layer resolves trackers through.
func TestInit_RegistersDepsGetTrackers(t *testing.T) {
	if deps.GetTrackers == nil {
		t.Fatal("deps.GetTrackers is nil; the tracker init() did not register the factory")
	}
}
