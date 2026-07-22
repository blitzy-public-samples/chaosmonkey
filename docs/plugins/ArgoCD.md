# Argo CD

Chaos Monkey can optionally coordinate with [Argo CD](https://argo-cd.readthedocs.io/)
(GitOps) so that it does not inject failures into workloads that Argo CD currently
considers unstable, and so that chaos actions become visible in the Argo CD audit
trail.

## Overview

The integration adds two capabilities:

1. A pre-flight **sync/health gate** that *skips* terminating any instance whose
   governing Argo CD `Application` is not both `Synced` and `Healthy`.
1. A best-effort **event write-back** that annotates the `Application` so the chaos
   action shows up in the Argo CD UI / timeline.

The integration is **client-side only**: it performs read-only status queries plus
a best-effort annotation write. It never triggers Argo CD syncs, rollbacks, or
rollouts, and it never modifies Argo CD itself.

The feature is **opt-in and disabled by default**. When it is not configured it is
completely inert, and existing Chaos Monkey behavior is preserved.

## Enabling the plugin

To turn on the Argo CD integration:

1. Set `argocd.enabled = true` and configure the `[argocd]` section (see the
   Configuration section below).
1. To also record chaos actions back to Argo CD, add `"argocd"` to the
   `chaosmonkey.trackers` list, i.e. `trackers = ["argocd"]`.

Like the other extension points, the Argo CD plugin registers itself through a Go
`init()` function and is loaded through a blank import in
`cmd/chaosmonkey/main.go`, matching the existing outage / tracker / constrainer
plugin registration pattern. See the [Plugins](index.md) page for how to build a
custom version of Chaos Monkey with plugins.

## Configuration

All configuration lives under the optional `[argocd]` section of
`chaosmonkey.toml`:

- `enabled` — boolean, default `false`. Master switch for the Argo CD gate and
  write-back.
- `endpoint` — the Argo CD API server base URL, e.g. `https://argocd.example.com`.
- `token` — inline bearer token (JWT). Prefer `token_file` in production.
- `token_file` — path to a file containing the bearer token (preferred over the
  inline `token`).
- `project` — optional Argo CD project used to scope `Application` lookups.
- `applications` — list of chaos-eligible Argo CD `Application` names / selectors.
- `insecure_skip_verify` — boolean, default `false`. Skip TLS verification to the
  Argo CD server.
- `ca_cert` — path to a PEM CA bundle used to verify the Argo CD server
  certificate.
- `timeout` — per-request timeout in seconds, default `30`.

Example:

```toml
[argocd]
enabled = true
endpoint = "https://argocd.example.com"
token_file = "/etc/chaosmonkey/argocd-token"
project = "production"
applications = ["checkout", "payments"]
insecure_skip_verify = false
ca_cert = "/etc/chaosmonkey/argocd-ca.pem"
timeout = 30
```

See the [Configuration File Format](../Configuration-file-format) page for the full
`chaosmonkey.toml` reference.

## Gate behavior

The gate implements the
[Precheck](https://godoc.org/github.com/Netflix/chaosmonkey/#Precheck) interface and
is evaluated immediately before a termination executes.

- It **allows** a termination only when `status.sync.status == Synced` **and**
  `status.health.status == Healthy`.
- It **denies** (skips the experiment, *without* raising an error) when the
  `Application` sync/health is any of `Progressing`, `OutOfSync`, `Degraded`,
  `Suspended`, `Missing`, or `Unknown`.

The gate is **fail-closed**: an unreachable API, an authentication failure, a
deleted / renamed `Application`, or an `Application` that manages zero live
resources all result in a **skip** — the gate denies and never crashes the
scheduler.

The gate is **additive**. It is evaluated alongside (never in place of) the
existing safety controls — `enabled` / `leashed`, the outage checker, account
gating, `Exception` / whitelist opt-outs, and the min-time `Checker`. It can only
make Chaos Monkey *more* conservative, never less.

## Write-back (event annotation)

After a termination is recorded, the `argocd` tracker — an implementation of the
[Tracker](https://godoc.org/github.com/Netflix/chaosmonkey/#Tracker) interface,
registered in
[tracker/tracker.go](https://github.com/Netflix/chaosmonkey/blob/master/tracker/tracker.go)
— PATCHes a custom annotation onto the governing `Application`. For example, an
annotation `chaosmonkey.netflix.com/last-termination` carrying the instance id, an
RFC3339 timestamp, the termination id, and the leashed flag, so the chaos action
appears in the Argo CD UI / timeline.

The write-back is **best-effort and non-blocking**: on any failure it logs and
returns `nil`, so a write-back error never blocks or fails a termination. This is
required because the termination workflow otherwise treats a tracker error as
fatal.

Optionally, when the `argocd-notifications-controller` is installed, subscription
annotations of the form
`notifications.argoproj.io/subscribe.<trigger>.<service>: <recipient>` can surface
chaos events through configured notification services. The controller is optional —
the custom-annotation write-back works whether or not it is present.

## Authentication and TLS

Authentication uses a bearer JWT sent as `Authorization: Bearer <token>`, sourced
from either `token` (inline) or `token_file` (a path; preferred).

A token can be obtained via `POST /api/v1/session` (username / password) or via a
project-role token. The **project-role token is recommended** as the
least-privilege option for automation.

For TLS, verify the Argo CD server certificate by pointing `ca_cert` at a PEM CA
bundle, or set `insecure_skip_verify = true` to skip verification (this mirrors the
Spinnaker adapter's X509 path). Note that `insecure_skip_verify` is insecure and is
intended only for testing or self-signed endpoints.

## Edge cases

- When `argocd.enabled = false` or the `[argocd]` section is absent, the feature is
  fully **inert** and existing Chaos Monkey behavior is preserved byte-for-byte: the
  provider resolves to an allow-all gate / no-op tracker, exactly like the shipped
  no-op outage and constrainer providers.
- Target resolution maps a selected termination target to its governing Argo CD
  `Application` using `status.resources[]` and/or the `app.kubernetes.io/instance`
  tracking label. If no governing `Application` can be resolved, the gate fails
  closed (skips).

See the [Plugins](index.md) page for info on how to build a custom version of Chaos
Monkey with your plugin. For the full configuration reference, see the
[Configuration File Format](../Configuration-file-format) page.
