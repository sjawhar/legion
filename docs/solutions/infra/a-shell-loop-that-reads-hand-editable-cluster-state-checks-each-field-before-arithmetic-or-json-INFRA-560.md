---
title: "A shell loop that reads hand-editable cluster state checks each field before arithmetic or JSON, and matches keys literally"
category: infra
tags:
  - shell
  - kubernetes
  - annotations
  - merge-patch
date: 2026-10-27
status: active
module: packages/envoy/infra/gcp
related_issues:
  - "INFRA-560"
  - "sjawhar/legion#2368"
---

# A shell loop that reads hand-editable cluster state checks each field before arithmetic or JSON

- Before a number from an annotation reaches `$(( ))` or `-lt`, match it with `case`: refuse a
  leading zero (the shell reads `08` as octal and dash fails with "Illegal number: 09") and more
  digits than the field can hold (dash fails past 19, so bound it near the real width, 12 for an
  epoch). In a background loop that error ends the loop for good.
- Escape regex metacharacters in a key before matching it with `sed` (the dots of
  `legion.dev/...`), so a lookalike key is not read in place of the real one.
- Drop an entry whose text holds anything outside the character set you write, rather than copy it
  into a hand-built merge patch: a backslash or quote makes the JSON invalid, and the API server
  then refuses every later write.
- Write with the `resourceVersion` the pass read in the merge patch, so a concurrent writer's
  change makes this write fail with 409 and change nothing.
- Run the script tests under dash and bash (`ENVOY_TEST_SHELL`), and under busybox when the
  image's tools are busybox.

## Evidence

- In sjawhar/legion#2368 the front door's claim gate (`packages/envoy/infra/gcp/dispatch_frontdoor.go`,
  `annotation`, `entry`, `patch`) died on an epoch `09`. The tester's red test
  `TestDispatchCertificateBudgetSkipsAMalformedEpoch` showed it; `022ff4be` fixed it with
  `TestDispatchCertificateBudgetReadsOnlyItsOwnAnnotation` (a `legionxdev/...` key) and
  `TestDispatchCertificateBudgetDropsAnEntryItCannotWrite` (a 400 for the invalid patch).
