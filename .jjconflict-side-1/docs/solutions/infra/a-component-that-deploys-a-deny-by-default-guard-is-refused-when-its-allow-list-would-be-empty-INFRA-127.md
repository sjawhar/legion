---
title: "A component that deploys a deny-by-default guard is refused when its allow-list would be empty, names the post-deploy check nothing machine-checks, and allow-lists only names you control"
category: infra
tags:
  - pulumi
  - allow-list
  - deny-by-default
  - operator-document
  - preview-refusal
  - dns-rebinding
date: 2026-10-22
status: active
module: envoy-infra
related_issues:
  - "INFRA-127"
  - "sjawhar/legion#2108"
---

# A component that deploys a deny-by-default guard is refused when its allow-list would be empty

Four rules for a component whose rendered configuration decides who a deployed binary serves.

- **A binary that gains a deny-by-default guard changes every component that deploys it, in the
  same release, and the component refuses at preview the configuration it would render nothing
  from.** Not a default, not warn-first: an unset allow-list renders a byte-identical environment
  to the one that worked before the guard existed, so no diff, no golden, no health check and no
  type is any different. The refusal is the only layer that can notice.
- **Validate the caller's own list before you compose your own entries into it, under the caller's
  own path.** A component that prepends an address of its own and then checks the whole list
  reports the operator's first entry by the rendered list's index — an index they never wrote. The
  shared checker takes the input path as a parameter (`CheckAllowedHosts(input string, hosts
  []string)`), so each caller names its own: `allowedHosts` for a listener's own config,
  `listener.allowedHosts` for the component that renders entries beside the operator's.
- **When the configured value is one nothing can validate, the operator document owns the check,
  and the check is exercised before it is written down.** A name that matches its shape rule
  without being the real one previews, applies and leaves a healthy deployment that serves nobody.
  Name the exact log line or the exact request, and say why the deploy proves neither.
- **Allow-list only names you control or the network's own.** Where the guard is the whole access
  control — no bearer, no verifier — an entry in a zone someone else controls hands a page that
  rebinds that name a same-origin route into an uncredentialled API. An operator document that
  invites "a name of your own" invites exactly that; say which names are safe to add.

## Evidence (INFRA-127, sjawhar/legion#2108)

`packages/envoy/cmd/listener/apiauth.go` grew a `Host` guard: with `open := token == "" &&
verifier == nil`, a request whose `Host` is neither loopback nor a member of `ENVOY_ALLOWED_HOSTS`
is answered 421. `core.ListenerConfig.Env` renders that variable only when the list is non-empty,
and `gcp:NatsHub` set nothing, so the hub's listener was configured to serve nobody from the
moment its image moved — 92 of 107 live sessions reached that hub over the tailnet. No preview, no
golden and no health check differed, and the author of the guard had recorded the consequence on
the day it merged; it reached no configuration.

The fix refuses at preview: `gcp/natshub.go`'s `resolveListener` returns
`tailnetDnsName is required for a hub with a listener: …` before anything registers. The tester
drove it through `Construct`, the call `pulumi preview` makes, and observed 0 registrations on the
refusal against 11 once the line is added. The rollout is an immediate refusal, not warn-first:
it costs the one consumer a line and closes a configuration that silently takes every session on
the network off messaging.

The index provenance was a `ce-simplify-code` finding two reviewers raised independently: the hub
prepended `core.TailnetHostName(hostname, tailnetDnsName)` before the per-entry check, so an
operator's first `listener.allowedHosts` entry was refused as `allowedHosts[1]`, contradicting the
same change's operator document. `core.CheckAllowedHosts(input, hosts)` now runs on the operator's
list alone, first, as `core.CheckMachineID(input, id)` already did.

Nothing machine-checks `tailnetDnsName`: the managed instance group's autohealing check reads
NATS's `/healthz` on 8222, and nothing in the repository dials 9020. A value matching
`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.ts\.net$` without being this tailnet's name — or a MagicDNS
collision handing the node `<h>-1` — leaves a healthy group whose listener answers 421 to
everyone, this issue's own outage reached by a typo. `docs/nats-hub.md`'s "Updating a hub" now
names the check: read the listener's `listener API hosts:` boot line
(`describeListenerAPIHosts`, `cmd/listener/apiauth.go`), or dial `/v1` at
`http://<h>.<tailnetDnsName>:9020` for a 200. It was exercised before it was written: the tester's
rendered-hub run logged `listener API hosts: loopback, example-hub.example-tailnet.ts.net`, and the
control with the list dropped logged `listener API hosts: loopback`.

The trust-model rule is the reviewer's second hardening note, recorded here on the architect's
ruling rather than spent on another review round, because a risk recorded only in a merged pull
request body reaches nobody. For this listener the tailnet ACL on 9020 plus `ENVOY_ALLOWED_HOSTS`
are the whole control on `/v1`: the guard exists so that a page which rebinds a name of its own to
the listener's address is refused, and it keeps that property only while every entry is a name the
deployment controls or the tailnet's own (`<h>.<tailnetDnsName>`, its bare MagicDNS label, its
tailnet IP). An entry in a zone the deployment does not control gives that page the same-origin
route the guard was added to close. The entries are shape-checked and never scoped, so this is the
operator's to get right and the document's to say.
