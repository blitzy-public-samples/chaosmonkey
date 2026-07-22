![logo](docs/logo.png "logo")

[![NetflixOSS Lifecycle](https://img.shields.io/osslifecycle/Netflix/chaosmonkey.svg)](OSSMETADATA) [![Build Status][travis-badge]][travis] [![GoDoc][godoc-badge]][godoc] [![GoReportCard][report-badge]][report]

[travis-badge]: https://travis-ci.com/Netflix/chaosmonkey.svg?branch=master
[travis]: https://travis-ci.com/Netflix/chaosmonkey
[godoc-badge]: https://godoc.org/github.com/Netflix/chaosmonkey?status.svg
[godoc]: https://godoc.org/github.com/Netflix/chaosmonkey
[report-badge]: https://goreportcard.com/badge/github.com/Netflix/chaosmonkey
[report]: https://goreportcard.com/report/github.com/Netflix/chaosmonkey

Chaos Monkey randomly terminates virtual machine instances and containers that
run inside of your production environment. Exposing engineers to
failures more frequently incentivizes them to build resilient services.

See the [documentation][docs] for info on how to use Chaos Monkey.

Chaos Monkey is an example of a tool that follows the
[Principles of Chaos Engineering][PoC].

[PoC]: http://principlesofchaos.org/

### Requirements

This version of Chaos Monkey is fully integrated with [Spinnaker], the
continuous delivery platform that we use at Netflix. You must be managing your
apps with Spinnaker to use Chaos Monkey to terminate instances.

Chaos Monkey should work with any backend that Spinnaker supports (AWS, Google
Compute Engine, Azure, Kubernetes, Cloud Foundry). It has been tested with
AWS, [GCE][gce-blogpost], and Kubernetes.

### Argo CD integration (optional)

Chaos Monkey can optionally coordinate with [Argo CD] so it does not disrupt
workloads that GitOps currently considers unstable. An operator marks specific
Argo CD `Application` resources as chaos-eligible; when a termination target
correlates to one of those Applications through its managed resources, Chaos
Monkey permits the experiment only while that `Application` is both `Synced` and
`Healthy` with a live managed workload. Otherwise it **fails closed** — on any
error, missing Application, ambiguity, or uncertain ownership it skips the
experiment rather than risk an unsafe termination. It can additionally write a
best-effort annotation back to the `Application` recording the chaos action;
that annotation is stored on the `Application` and is visible through the Argo
CD API and the Application's resource/manifest (annotations) view. Surfacing it
as a notification additionally requires a custom Argo CD Notifications trigger
and template. The gate is **additive** to Chaos Monkey's existing safety
controls. The feature is **opt-in and disabled by default**; when it is not
configured, Chaos Monkey behaves exactly as before. See the [Argo CD plugin
documentation][argocd-docs] for configuration details.

### Install locally

To install the Chaos Monkey binary on your local machine:

```
go get github.com/netflix/chaosmonkey/cmd/chaosmonkey
```

### How to deploy

See the [docs] for instructions on how to configure and deploy Chaos Monkey.

### Support

[Simian Army Google group](http://groups.google.com/group/simianarmy-users).

[Spinnaker]: http://www.spinnaker.io/
[docs]: https://netflix.github.io/chaosmonkey
[gce-blogpost]: https://medium.com/continuous-delivery-scale/running-chaos-monkey-on-spinnaker-google-compute-engine-gce-155dc52f20ef
[Argo CD]: https://argo-cd.readthedocs.io/
[argocd-docs]: https://netflix.github.io/chaosmonkey/plugins/ArgoCD/
