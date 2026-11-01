---
title: "A row class that reuses its sibling's assertion names a mechanism it never checks"
category: testing
tags:
  - table-tests
  - refusals
  - mutation
  - review
date: 2026-10-30
status: active
module: packages/envoy
related_issues:
  - "INFRA-797"
  - "sjawhar/legion#2553"
---

# A row class that reuses its sibling's assertion names a mechanism it never checks

Extends docs/solutions/testing/a-refusal-tables-negative-row-passes-every-check-but-its-own-so-it-pins-the-message-not-the-judges-order-LEGION-629.md.

That document says a negative row must pin the message, and must satisfy every check before the
one it targets. This is the shape that tells you a row does neither, readable before any mutation
runs:

- **the row's name names a mechanism no assertion of it mentions.** A row called
  `…_tailscaleImage_malformed` whose body asserts only "the error names the group's other unset
  inputs" tests nothing about a malformed image;
- **a new row class varies the input and reuses the sibling class's assertion verbatim.** Two
  classes that assert the same predicate over different inputs are one class, and the second
  proves only what the first already did.

Both are visible in review. When you see either, mutate the thing the row names — remove the
decoration it should carry, then force the check it targets to pass — and keep the output. A row
that survives either mutation is not a row.

A derived assertion is the other way this fails open: when the row lists by hand what its setup
assigns and the assertion is "the group minus that list", a name wrongly added to the list
deletes an assertion and nothing goes red.

## Evidence

INFRA-797 added four `…_malformed` rows to
`TestDispatchPartialTailnetGroupNamesEveryMissingInput`
(`packages/envoy/infra/gcp/dispatch_sandbox_test.go`) to pin that a refusal of a set-but-invalid
input still names the group's unset inputs. Each asserted only the predicate the missing-input
rows beside them already asserted. The reviewer removed the clause from the `signInDomain`
refusal and the row stayed green; a wider mutation — the clause dropped from three more refusals
and one check reordered — left all four green. The row's own setup was the reason: a positional
`valid: 0` prefix left `signInClientId` unset, so `signInClientId is required` fired and the
domain check never ran.

The rows now give explicit setup and carry a `want` column holding the resolver's own words
(`dispatch_sandbox_test.go:315, 325, 336, 348, 361`), asserted beside the unset list. Under the
same two mutations, five subtests fail where none did.
