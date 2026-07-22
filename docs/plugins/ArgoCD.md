# Argo CD

Chaos Monkey can optionally coordinate with [Argo CD](https://argo-cd.readthedocs.io/)
(GitOps) so that it does not inject failures into workloads that Argo CD currently
considers unstable, and so that chaos actions are recorded as an annotation on the
governing Argo CD `Application`.

## Overview

The integration adds two capabilities:

1. A pre-flight **sync/health gate** that permits a termination only when the
   target correlates to a chaos-eligible Argo CD `Application` managing a live
   workload, and that `Application` is both `Synced` and `Healthy`; otherwise it
   *skips* the experiment (fail-closed).
1. A best-effort **event write-back** that records the chaos action as an
   annotation on the governing `Application`. That annotation is visible through
   the Argo CD API and the Application's resource/manifest (annotations) view;
   surfacing it as a notification additionally requires a custom Argo CD
   Notifications trigger and template.

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

- `enabled` — boolean, default `false`. Master switch for the Argo CD sync/health
  gate. It does not by itself enable write-back; the write-back is additionally
  activated by adding `"argocd"` to `chaosmonkey.trackers`.
- `endpoint` — the Argo CD API server base URL, e.g. `https://argocd.example.com`.
- `token` — inline bearer token (JWT). Prefer `token_file` in production.
- `token_file` — path to a file containing the bearer token (preferred over the
  inline `token`).
- `project` — optional Argo CD project used to scope `Application` lookups.
- `applications` — list of **exact** chaos-eligible Argo CD `Application` names,
  each matched against an Application's `metadata.name`. Label-selector–based
  discovery via the Argo CD list API is a concern of the Argo CD client layer,
  not this key. An empty list makes no Application eligible, so the gate then
  denies every experiment.
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
[Precheck](https://pkg.go.dev/github.com/Netflix/chaosmonkey/v2#Precheck) interface
and is evaluated immediately before a termination executes.

- It **allows** a termination only when the target correlates to a chaos-eligible
  `Application` (one whose exact name is listed in `argocd.applications`) that
  manages exactly one live workload matching the target, **and** that
  `Application` is both `status.sync.status == Synced` **and**
  `status.health.status == Healthy`.
- It **denies** (skips the experiment, *without* raising an error) whenever the
  `Application` sync/health is any of `Progressing`, `OutOfSync`, `Degraded`,
  `Suspended`, `Missing`, or `Unknown`, when the target is not managed by any
  eligible `Application`, or when ownership is ambiguous (more than one candidate
  live workload).

The gate is **fail-closed**: an unreachable API, an authentication failure, a
deleted / renamed `Application`, an ineligible or ambiguous target, or an
`Application` that manages zero live workloads all result in a **skip** — the gate
denies and never crashes the scheduler.

The gate is **additive**. It is evaluated alongside (never in place of) the
existing safety controls — `enabled` / `leashed`, the outage checker, account
gating, `Exception` / whitelist opt-outs, and the min-time `Checker`. It can only
make Chaos Monkey *more* conservative, never less.

## Write-back (event annotation)

After a termination is recorded, the `argocd` tracker — an implementation of the
[Tracker](https://pkg.go.dev/github.com/Netflix/chaosmonkey/v2#Tracker) interface,
registered in
[tracker/tracker.go](https://github.com/Netflix/chaosmonkey/blob/master/tracker/tracker.go)
— PATCHes a custom annotation onto the governing `Application`. For example, an
annotation `chaosmonkey.netflix.com/last-termination` carrying the instance id, an
RFC3339 timestamp, the termination id, and the leashed flag. That annotation is
stored on the `Application` and is visible through the Argo CD API and the
Application's resource/manifest (annotations) view. It is **not** automatically
added to Argo CD's deployment history or event timeline; surfacing it that way
requires the Notifications configuration described below.

The write-back is **best-effort and non-blocking**: on any failure it logs and
returns `nil`, so a write-back error never blocks or fails a termination. This is
required because the termination workflow otherwise treats a tracker error as
fatal.

Optionally, when the `argocd-notifications-controller` is installed, chaos events
can be surfaced through Argo CD Notifications. This takes more than a subscription:
a subscription annotation of the form
`notifications.argoproj.io/subscribe.<trigger>.<service>: <recipient>` only attaches
a recipient to an **existing** trigger, so you must also configure a custom
**trigger** and **template** (in the `argocd-notifications-cm` ConfigMap) that
inspect the `chaosmonkey.netflix.com/last-termination` annotation and render the
message. The controller and this trigger/template configuration are optional — the
custom-annotation write-back works whether or not they are present.

## Authentication and TLS

Authentication uses a bearer JWT sent as `Authorization: Bearer <token>`, sourced
from either `token` (inline) or `token_file` (a path; preferred). A token can be
obtained via `POST /api/v1/session` (username / password) or, preferably, via a
**project-role token**, which is the least-privilege option for automation.

### Least-privilege RBAC

Grant the token only the permissions the integration actually uses. Argo CD
authorizes `get` and `update` on `applications` separately, so scope each to the
specific project (and, where possible, the specific Applications) and grant nothing
else — in particular **do not** grant `sync`, `override`, `action`, or `delete`,
none of which the integration performs.

- The **gate** needs read access only: `applications, get`.
- The **write-back** (only if you enable `trackers = ["argocd"]`) additionally
  needs `applications, update` so it can PATCH the annotation. `update` is powerful
  — it permits modifying Application spec and metadata — so restrict it to the
  intended project/Applications, and omit it entirely if you do not use the
  write-back.

Example project-role policy (in an `AppProject`) scoped to the `production`
project — read-only for the gate, plus a narrowly scoped `update` for write-back:

```yaml
# AppProject.spec.roles[]
- name: chaosmonkey
  policies:
    # gate: read Application sync/health/resources
    - p, proj:production:chaosmonkey, applications, get, production/*, allow
    # write-back only: annotate the Application (omit if write-back is disabled)
    - p, proj:production:chaosmonkey, applications, update, production/*, allow
```

Generate the token for that role with
`argocd proj role create-token production chaosmonkey`.

### Operational security

- **Protect the token file.** Store the credential in `token_file` (rather than
  inline in a committed config) and restrict it to owner-only permissions
  (`chmod 0600`, owned by the Chaos Monkey service account).
- **Use HTTPS only.** Point `endpoint` at an `https://` URL so the bearer token is
  never transmitted in cleartext.
- **Rotate regularly.** Project-role tokens support an expiry (`--expires-in`);
  rotate them on a schedule and immediately after any suspected exposure.
- **Never log the token.** Chaos Monkey does not log the credential — the config's
  string representation redacts it — and you should likewise keep it out of wrapper
  scripts, shell history, and CI logs.

### TLS

For TLS, verify the Argo CD server certificate by pointing `ca_cert` at a PEM CA
bundle. **Production deployments that use a self-signed or private-CA certificate
must supply that CA via `ca_cert`.** Setting `insecure_skip_verify = true` disables
certificate verification entirely, which exposes the bearer token to
man-in-the-middle interception; reserve it strictly for temporary local testing and
never use it in production.

## Edge cases

- When `argocd.enabled = false` or the `[argocd]` section is absent, the feature is
  fully **inert** and existing Chaos Monkey behavior is preserved byte-for-byte: the
  provider resolves to an allow-all gate / no-op tracker, exactly like the shipped
  no-op outage and constrainer providers.
- Target resolution is authoritative and anchored to the operator's allow-list:
  only Applications whose exact `metadata.name` is listed in `argocd.applications`
  are considered, and the selected target must correlate to exactly one live
  managed workload in that Application's `status.resources[]`. Chaos Monkey does
  not infer eligibility from the `app.kubernetes.io/instance` tracking label;
  label-selector–based Application listing via the Argo CD list API is a concern of
  the Argo CD client layer. If no eligible `Application` manages the target, or
  ownership is ambiguous, the gate fails closed (skips).

See the [Plugins](index.md) page for info on how to build a custom version of Chaos
Monkey with your plugin. For the full configuration reference, see the
[Configuration File Format](../Configuration-file-format) page.
