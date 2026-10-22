---
title: "A configuration projected onto an unauthenticated route is audited by the loader's admitted value shapes, not by field names: plant a credential where you emit, mask URLs from their parsed parts, and reuse the sanitizer the tree already has"
category: daemon
tags:
  - settings
  - allowlist
  - credential
  - url-userinfo
  - unauthenticated-route
  - golden-fixture
  - security-review
date: 2026-10-21
status: active
module: packages/daemon/internal/api
related_issues:
  - "LEGION-791"
  - "sjawhar/legion#2096"
---

# A configuration projected onto an unauthenticated route is audited by the loader's admitted value shapes, not by field names

- An allowlist over configuration *fields* classifies the field, never the value it carries. A
  field rightly classified "emitted" still leaks when the loader admits a credential inside its
  value. Before the first push of any projection of configuration onto a route, write a table
  with one row per emitted string: the key, the loader function that reads it, the widest value
  that function admits (userinfo? query? fragment? arbitrary operator text?), and the projection's
  treatment (masked; as written because the loader refuses the shape, naming where; or an accepted
  risk, named in the pull request's `Not proven / risk`). Every row whose loader admits a credential
  and whose treatment says "as written" is a leak or a recorded acceptance, never a default.
- Read the loader, not the key's name: in this daemon `endpointURL` tells operators to put NATS and
  Envoy credentials in URL userinfo (`internal/config/config.go`, "use URL userinfo or a secret for
  credentials"), `readPluginSource` stores an http(s) tarball URL whole — userinfo, query and
  fragment — and `readAgentSecrets` refuses all four shapes on `agent_secrets.url`. Three validators
  for three URL-shaped keys, three different answers; the disagreement is the audit list.
- Mask a URL by composing from its parsed parts — scheme, `xxxxx@` when it carries userinfo, host,
  and the path when the setting's reader needs one — never by editing the raw string, so a query
  or a fragment cannot survive either. Fail safe: an input that yields no scheme and host is
  `xxxxx` whole, never the raw value.
- Before writing a sanitizer, grep for one that already handles the same value. This tree had
  `internal/runtime/sandbox/addresses.go`'s `namedURL`, with a test that already pinned userinfo
  and a `?token=` query; its existence was the finding. Reuse it, or diverge in writing: a comment
  on each naming the other and why the output differs (a log name keeps the host alone; a setting
  keeps the path its reader acts on).
- A planted-value sweep proves only what the fixture plants. Plant a credential in every emitted
  field whose loader admits one — not only in the fields the allowlist excludes — assert each
  planted literal absent from the marshalled bytes, and make the credential-planted file the input
  of the committed golden, so the fixture's bytes are a standing absence assertion a second
  language re-checks. Hosts are reserved example names, paths fixed absolute literals.
- Never let a doc comment carry a universal the tests do not derive ("each address rides the
  mask"). Name the rule's exceptions explicitly, or derive the enumeration in the test that already
  walks the type.

## Evidence

sjawhar/legion#2096 (LEGION-791) added `settings` — the daemon's resolved `legion.yaml` with every
credential left out — to the unauthenticated `GET /legion/v1/state`. `api.SettingsOf` was an
allowlist held by a reflect test over every `config.Config` field, the kubernetes fixture planted a
PEM, a DSN password, every `*_file` path and each provider key's store key, and the sweep found
none of them on the wire. Two review rounds found two leaks the same way:

- Round 1: `daemon_url`, `envoy_url`, every `nats_urls` entry and `dispatch_url` rode verbatim; the
  loader itself names their userinfo as the credential location. Every URL the fixture planted
  carried no userinfo, so the sweep was green; the tester's independent file widened along the
  excluded axis and reproduced the blind spot.
- Round 2: `runtime.kubernetes.plugins.pi_legion`/`pi_envoy`, admitted as an http(s) URL stored
  whole, rode verbatim — a private or pre-signed tarball's basic-auth userinfo or `X-Amz-Signature`
  query. The fixture used the version form `10.4.0`, so no test exercised the URL form.

The fix was one helper composing from parsed parts (`endpointOf`), a shape test mirroring the
loader's for the plugin sources, planted userinfo and a planted query in the fixture and in Stage
1's live `legion.yaml` (`envoy_url: http://legion:<password>@127.0.0.1:9020`, asserted served as
`http://xxxxx@127.0.0.1:9020`), the planted literals in the sweep, and the pre-fix binary re-run on
the same file as the negative control (it served the passwords). Applying the four-column table to
the diff after the fact finds both leaks in one pass and nothing else; the reviewer's recorded
remainders are the operator-authored argv and text keys (`omp_launch_prefix`,
`runtime.kubernetes.setup`, `capabilities.decided`, `merge.by_hand`), accepted and named.
