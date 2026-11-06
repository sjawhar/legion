---
title: "A cross-package golden join compares the decoded value whole, because a tolerant decoder makes \"a fact decoded\" vacuous"
category: testing
tags:
  - golden-fixtures
  - contract-test
  - cross-package
  - decoder
date: 2026-11-02
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2675"
---

# A cross-package golden join compares the decoded value whole, because a tolerant decoder makes "a fact decoded" vacuous

Where one package writes a golden and another decodes it, the join test is the only machine that
holds the two packages to one wire shape. It holds them to exactly as much as its assertion names.

- **Assert the decoded value by equality against the golden's own fields**, not that decoding
  produced something. A decoder that reads a map and ignores a missing key returns a value for
  every subset of the fields it reads, so "a fact decoded" passes while the producer drops any
  field the decoder tolerates.
- **Read the decoder for the fields it takes with a discarded error** before you decide what to
  assert: those are exactly the fields the weak assertion leaves unprotected.
- **Take each expected field from the golden rather than from a literal**, through a helper that
  fails by name when the golden lacks it. The failure then names the key the producer dropped
  instead of printing two structs.
- **The assertion is free of a second fixture.** The golden the producer's own test regenerates is
  the input; strengthening the consumer's read of it adds no file to keep in step.

## Evidence (sjawhar/legion#2675, INFRA-609)

`packages/contracts/fixtures/github-envelopes/push.json` is written by the Envoy listener's golden
test and read by the Legion daemon's intake test, which is the only place the two packages' idea
of a push envelope meets.

`decodePush` (`packages/daemon/internal/intake/decode.go:714-727`) reads a
`map[string]json.RawMessage` and takes `before`, `pusher` and `sender` with `, _`: a missing key
is not an error by construction, and only `ref` and `after` can make it return no fact.
`TestEnvoyGoldenSubjectsReachTheirRepositorysConsumer` asserted `decoded.Fact != nil`
(`packages/daemon/internal/intake/intake_test.go:844-846`, kept). So the listener could have
stopped sending any of those three and the join would have stayed green while the daemon silently
lost the field — the daemon reads `sender` to tell a synchronize's pusher, and `before` to name
the head a push replaced.

The change that removed three *unread* fields from that envelope added the assertion the join was
missing (`intake_test.go:847-871`): the decoded `intake.Push` is compared whole with a `want` whose
`Before`, `After`, `Pusher` and `Sender` are taken from the golden's own payload through a `field`
closure that fails naming the key. The control: `"pusher"` deleted from the golden by hand gives
`intake_test.go:866: the push golden carries no string "pusher", which decodePush reads`, raised
after the pre-existing `decoded.Fact != nil` check had already passed, which is what the
unextended test would have done with that golden.
