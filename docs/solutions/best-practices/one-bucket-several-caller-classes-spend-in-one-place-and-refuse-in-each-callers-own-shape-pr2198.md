---
title: "One bucket, several caller classes: spend it in one place and refuse in each caller's own shape"
category: best-practices
tags:
  - rate-limiting
  - authentication
  - http-headers
  - broker
date: 2026-11-01
status: active
module: packages/envoy/internal/broker/api
related_issues:
  - "sjawhar/legion#2198"
---

# One bucket, several caller classes: spend it in one place and refuse in each caller's own shape

Extends docs/solutions/best-practices/a-route-that-verifies-before-it-limits-lets-a-stranger-spend-your-upstream-INFRA-228.md.

That rule puts the bucket before the credential check. When a second kind of caller joins the
same bucket — a browser beside a service, a public route beside an authenticated one — the spend
is shared and the refusal is not.

- **Split the spend from the refusal.** One function answers whether the address had a request
  left; each class writes its own answer. Reusing the first class's refusal writer tells the new
  caller about a credential it never sent, and skips whatever that class's answers carry.
- **A browser's refusal is one of its own pages**, under every header the other answers of that
  page carry. A security header set that holds for 200 and 403 and not for 429 is a gap no test
  of the happy path finds.
- **Moving a route off a shared wrapper drops everything the wrapper did**: the bucket, the
  power check, and the census test that filters on that wrapper. Enumerate what the wrapper
  carried, re-add each in the handler, and correct any sentence claiming the census covers the
  moved row.

## Evidence

In sjawhar/legion#2198 the broker's approval page joined the bucket every route a ServiceAccount
token authenticates shares. The first attempt spent the bucket and reused `refuseAccountAddress`,
so a person who flooded `GET /approve/<record>` got `{"code":"RATE_LIMITED","error":"too many
requests with a ServiceAccount token; try again later"}` with no `Content-Security-Policy`,
`X-Frame-Options`, `Referrer-Policy` or `Cache-Control`, against the spec's "each answer of a page
route, a redirect included, has …". No test drove a page route past the limit, so deleting either
call would have passed every suite.

The fix is `accountAddressAllows`, which spends and reports, with three refusals over it: the
page's own HTML 429 (`approval.Page.WriteRateLimited`), JSON naming the address on the public
`GET /signin/status`, and the ServiceAccount wording only where a token was really sent.
`TestAFloodedPageAnswersThePagesOwn429AndNamesNoToken` drives a burst of 3 and reads the headers.

The same change moved the two decide routes off `accountAuth` to a wrapper whose handler
authenticates after it has read the record's kind. That move lost the per-address bucket, which
was restored inside the handler, and later lost the routes from
`TestEveryAccountRouteDeclaresItsPower`, which filters on the `authAccount` class;
`cmd/broker/account_powers_test.go` is what pins them now, and `packages/envoy/AGENTS.md` says so.
